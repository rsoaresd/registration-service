package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/codeready-toolchain/registration-service/pkg/auth"
	"github.com/codeready-toolchain/registration-service/pkg/namespaced"
	"github.com/codeready-toolchain/registration-service/pkg/proxy/handlers"
	"github.com/codeready-toolchain/registration-service/pkg/proxy/metrics"
	proxytest "github.com/codeready-toolchain/registration-service/pkg/proxy/test"
	"github.com/codeready-toolchain/registration-service/pkg/signup"
	"github.com/codeready-toolchain/registration-service/test"
	"github.com/codeready-toolchain/registration-service/test/fake"
	"github.com/codeready-toolchain/registration-service/test/util"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/kubernetes/scheme"

	toolchainv1alpha1 "github.com/codeready-toolchain/api/api/v1alpha1"
	commoncluster "github.com/codeready-toolchain/toolchain-common/pkg/cluster"
	"github.com/codeready-toolchain/toolchain-common/pkg/hash"
	commontest "github.com/codeready-toolchain/toolchain-common/pkg/test"
	authsupport "github.com/codeready-toolchain/toolchain-common/pkg/test/auth"
	testconfig "github.com/codeready-toolchain/toolchain-common/pkg/test/config"

	routev1 "github.com/openshift/api/route/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type TestProxySuite struct {
	test.UnitTestSuite
}

func TestRunProxySuite(t *testing.T) {
	suite.Run(t, &TestProxySuite{test.UnitTestSuite{}})
}

var (
	bannedUser = toolchainv1alpha1.BannedUser{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alice",
			Namespace: commontest.HostOperatorNs,
			Labels: map[string]string{
				toolchainv1alpha1.BannedUserEmailHashLabelKey: hash.EncodeString("alice@redhat.com"),
			},
		},
		Spec: toolchainv1alpha1.BannedUserSpec{
			Email: "alice@redhat.com",
		},
	}

	bannedUserListErrorEmailValue = "banneduser-list-error"
)

func (s *TestProxySuite) TestProxy() {
	// given

	env := s.DefaultConfig().Environment()
	defer s.SetConfig(testconfig.RegistrationService().
		Environment(env))
	s.SetConfig(testconfig.RegistrationService().
		Environment(string(testconfig.E2E))) // We use e2e-test environment just to be able to re-use token generation
	_, err := auth.InitializeDefaultTokenParser()
	require.NoError(s.T(), err)

	for _, environment := range []testconfig.EnvName{testconfig.E2E, testconfig.Dev, testconfig.Prod} {
		s.Run("for environment "+string(environment), func() {

			s.SetConfig(testconfig.RegistrationService().
				Environment(string(environment)))

			fakeClient, app := util.PrepareInClusterApp(s.T(), &bannedUser)
			fakeClient.MockList = func(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
				listOptions := &client.ListOptions{}
				for _, opt := range opts {
					opt.ApplyToList(listOptions)
				}
				if strings.Contains(listOptions.LabelSelector.String(), hash.EncodeString(bannedUserListErrorEmailValue)) {
					return fmt.Errorf("list banned user error")
				}
				return fakeClient.Client.List(ctx, list, opts...)
			}
			nsClient := namespaced.NewClient(fakeClient, commontest.HostOperatorNs)

			proxyMetrics := metrics.NewProxyMetrics(prometheus.NewRegistry())
			proxy, err := NewProxy(nsClient, app, proxyMetrics, proxytest.NewGetMembersFunc(commontest.NewFakeClient(s.T())))
			require.NoError(s.T(), err)

			server := proxy.StartProxy(DefaultPort)
			require.NotNil(s.T(), server)
			defer func() {
				_ = server.Close()
			}()

			s.Run("is alive", func() {
				s.waitForProxyToBeAlive(DefaultPort)
			})
			s.Run("health check ok", func() {
				s.checkProxyIsHealthy(DefaultPort)
			})

			s.checkPlainHTTPErrors(proxy)
			s.checkWebsocketsError()
			s.checkWebLogin()
			s.checkProxyOK(proxy)
		})
	}
}

func (s *TestProxySuite) spinUpProxy(port string) (*Proxy, *http.Server) {
	proxyMetrics := metrics.NewProxyMetrics(prometheus.NewRegistry())
	fakeClient, app := util.PrepareInClusterApp(s.T())
	proxy, err := NewProxy(namespaced.NewClient(fakeClient, commontest.HostOperatorNs),
		app, proxyMetrics, proxytest.NewGetMembersFunc(commontest.NewFakeClient(s.T())))
	require.NoError(s.T(), err)

	server := proxy.StartProxy(port)
	require.NotNil(s.T(), server)

	return proxy, server
}

func (s *TestProxySuite) waitForProxyToBeAlive(port string) {
	// Wait up to N seconds for the Proxy server to start
	ready := false
	sec := 10
	for i := 0; i < sec; i++ {
		log.Println("Checking if Proxy is started...")
		req, err := http.NewRequest("GET", fmt.Sprintf("http://localhost:%s/api/mycoolworkspace/pods", port), nil)
		require.NoError(s.T(), err)
		require.NotNil(s.T(), req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			// The server may be running but still not fully ready to accept requests
			time.Sleep(time.Second)
			continue
		}
		// Server is up and running!
		ready = true
		break
	}
	require.True(s.T(), ready, "Proxy is not ready after %d seconds", sec)
}

func (s *TestProxySuite) checkProxyIsHealthy(port string) {
	req, err := http.NewRequest("GET", fmt.Sprintf("http://localhost:%s/proxyhealth", port), nil)
	require.NoError(s.T(), err)
	require.NotNil(s.T(), req)

	// when
	resp, err := http.DefaultClient.Do(req)

	// then
	require.NoError(s.T(), err)
	require.NotNil(s.T(), resp)
	defer resp.Body.Close()
	assert.Equal(s.T(), http.StatusOK, resp.StatusCode)
	s.assertResponseBody(resp, `{"alive": true}`)
}

func (s *TestProxySuite) checkPlainHTTPErrors(proxy *Proxy) {
	s.Run("plain http error", func() {
		s.Run("unauthorized if no token present", func() {
			req, err := http.NewRequest("GET", "http://localhost:8081/api/mycoolworkspace/pods", nil)
			require.NoError(s.T(), err)
			require.NotNil(s.T(), req)

			// when
			resp, err := http.DefaultClient.Do(req)

			// then
			require.NoError(s.T(), err)
			require.NotNil(s.T(), resp)
			defer resp.Body.Close()
			assert.Equal(s.T(), http.StatusUnauthorized, resp.StatusCode)
			s.assertResponseBody(resp, "invalid bearer token: no token found: a Bearer token is expected")
		})

		s.Run("unauthorized if can't parse token", func() {
			// when
			req, err := http.NewRequest("GET", "http://localhost:8081/api/mycoolworkspace/pods", nil)
			require.NoError(s.T(), err)
			require.NotNil(s.T(), req)
			req.Header.Set("Authorization", "Bearer not-a-token")
			resp, err := http.DefaultClient.Do(req)

			// then
			require.NoError(s.T(), err)
			require.NotNil(s.T(), resp)
			defer resp.Body.Close()
			assert.Equal(s.T(), http.StatusUnauthorized, resp.StatusCode)
			s.assertResponseBody(resp, "invalid bearer token: unable to extract claims from token: token is malformed: token contains an invalid number of segments")
		})

		s.Run("unauthorized if can't extract claims from a valid token", func() {
			// when
			req, err := http.NewRequest("GET", "http://localhost:8081/api/mycoolworkspace/pods", nil)
			require.NoError(s.T(), err)
			require.NotNil(s.T(), req)
			req.Header.Set("Authorization", "Bearer "+s.token("unauthorized-user", authsupport.WithSubClaim("")))
			resp, err := http.DefaultClient.Do(req)

			// then
			require.NoError(s.T(), err)
			require.NotNil(s.T(), resp)
			defer resp.Body.Close()
			assert.Equal(s.T(), http.StatusUnauthorized, resp.StatusCode)
			s.assertResponseBody(resp, "invalid bearer token: unable to extract claims from token: token does not comply to expected claims: subject missing")
		})

		s.Run("unauthorized if can't extract email from a valid token", func() {
			// when
			req, err := http.NewRequest("GET", "http://localhost:8081/api/mycoolworkspace/pods", nil)
			require.NoError(s.T(), err)
			require.NotNil(s.T(), req)
			req.Header.Set("Authorization", "Bearer "+s.token("unauthorized-user", authsupport.WithEmailClaim("")))
			resp, err := http.DefaultClient.Do(req)

			// then
			require.NoError(s.T(), err)
			require.NotNil(s.T(), resp)
			defer resp.Body.Close()
			assert.Equal(s.T(), http.StatusUnauthorized, resp.StatusCode)
			s.assertResponseBody(resp, "invalid bearer token: unable to extract claims from token: token does not comply to expected claims: email missing")
		})

		s.Run("unauthorized if workspace context is invalid", func() {
			// when
			req := s.request()
			req.URL.Path = "http://localhost:8081/workspaces/myworkspace" // invalid workspace context
			require.NotNil(s.T(), req)

			// when
			resp, err := http.DefaultClient.Do(req)

			// then
			require.NoError(s.T(), err)
			require.NotNil(s.T(), resp)
			defer resp.Body.Close()
			assert.Equal(s.T(), http.StatusBadRequest, resp.StatusCode)
			s.assertResponseBody(resp, "unable to get workspace context: workspace request path has too few segments '/workspaces/myworkspace'; expected path format: /workspaces/<workspace_name>/api/...")
		})

		s.Run("empty set of member clusters", func() {
			// given
			origGetMembersFunc := proxy.getMembersFunc
			proxy.getMembersFunc = func(_ ...commoncluster.Condition) []*commoncluster.CachedToolchainCluster {
				return nil
			}
			defer func() {
				proxy.getMembersFunc = origGetMembersFunc
			}()
			req := s.request()

			// when
			resp, err := http.DefaultClient.Do(req)

			// then
			require.NoError(s.T(), err)
			require.NotNil(s.T(), resp)
			defer resp.Body.Close()
			assert.Equal(s.T(), http.StatusInternalServerError, resp.StatusCode)
			s.assertResponseBody(resp, "unable to get target cluster: user is not provisioned (yet)")
		})

		s.Run("internal error if accessing incorrect url", func() {
			// given
			req := s.request()
			req.URL.Path = "http://localhost:8081/metrics"
			require.NotNil(s.T(), req)

			// when
			resp, err := http.DefaultClient.Do(req)

			// then
			require.NoError(s.T(), err)
			require.NotNil(s.T(), resp)
			defer resp.Body.Close()
			assert.Equal(s.T(), http.StatusInternalServerError, resp.StatusCode)
		})

		s.Run("forbidden error if user is banned", func() {
			// given
			req, err := http.NewRequest("GET", "http://localhost:8081/api/mycoolworkspace/pods", nil)
			require.NoError(s.T(), err)
			require.NotNil(s.T(), req)
			token := s.token("alice", authsupport.WithSubClaim("alice"), authsupport.WithEmailClaim(bannedUser.Spec.Email))
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
			resp, err := http.DefaultClient.Do(req)

			// then
			require.NoError(s.T(), err)
			require.NotNil(s.T(), resp)
			defer resp.Body.Close()
			assert.Equal(s.T(), http.StatusForbidden, resp.StatusCode)
			s.assertResponseBody(resp, "user access is forbidden: user access is forbidden")
		})

		s.Run("internal error if error occurred while defining if the user is banned", func() {
			// given
			req, err := http.NewRequest("GET", "http://localhost:8081/api/mycoolworkspace/pods", nil)
			require.NoError(s.T(), err)
			require.NotNil(s.T(), req)
			token := s.token("alice", authsupport.WithSubClaim("alice"), authsupport.WithEmailClaim(bannedUserListErrorEmailValue))
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
			resp, err := http.DefaultClient.Do(req)

			// then
			require.NoError(s.T(), err)
			require.NotNil(s.T(), resp)
			defer resp.Body.Close()
			assert.Equal(s.T(), http.StatusInternalServerError, resp.StatusCode)
			s.assertResponseBody(resp, "user access could not be verified: could not define user access")
		})
	})
}

func (s *TestProxySuite) checkWebsocketsError() {
	s.Run("websockets error", func() {
		tests := map[string]struct {
			ProtocolHeaders []string
			ExpectedError   string
		}{
			"empty token": {
				ProtocolHeaders: []string{"base64url.bearer.authorization.k8s.io.,dummy"},
				ExpectedError:   "invalid bearer token: no base64.bearer.authorization token found",
			},
			"not a jwt token": {
				ProtocolHeaders: []string{"base64url.bearer.authorization.k8s.io.dG9rZW4,dummy"},
				ExpectedError:   "invalid bearer token: unable to extract claims from token: token is malformed: token contains an invalid number of segments",
			},
			"invalid token is not base64 encoded": {
				ProtocolHeaders: []string{"base64url.bearer.authorization.k8s.io.token,dummy"},
				ExpectedError:   "invalid bearer token: invalid base64.bearer.authorization token encoding: illegal base64 data at input byte 4",
			},
			"invalid token contains non UTF-8-encoded runes": {
				ProtocolHeaders: []string{fmt.Sprintf("base64url.bearer.authorization.k8s.io.%s,dummy", base64.RawURLEncoding.EncodeToString([]byte("aa\xe2")))},
				ExpectedError:   "invalid bearer token: invalid base64.bearer.authorization token: contains non UTF-8-encoded runes",
			},
			"no header": {
				ProtocolHeaders: nil,
				ExpectedError:   "invalid bearer token: no base64.bearer.authorization token found",
			},
			"empty header": {
				ProtocolHeaders: []string{""},
				ExpectedError:   "invalid bearer token: no base64.bearer.authorization token found",
			},
			"non-bearer header": {
				ProtocolHeaders: []string{"undefined"},
				ExpectedError:   "invalid bearer token: no base64.bearer.authorization token found",
			},
			"empty bearer token": {
				ProtocolHeaders: []string{"base64url.bearer.authorization.k8s.io."},
				ExpectedError:   "invalid bearer token: no base64.bearer.authorization token found",
			},
			"multiple bearer tokens": {
				ProtocolHeaders: []string{
					"base64url.bearer.authorization.k8s.io.dG9rZW4,dummy",
					"base64url.bearer.authorization.k8s.io.dG9rZW4,dummy",
				},
				ExpectedError: "invalid bearer token: multiple base64.bearer.authorization tokens specified",
			},
		}

		for k, tc := range tests {
			s.Run(k, func() {
				req, err := http.NewRequest("GET", "http://localhost:8081/api/mycoolworkspace/pods", nil)
				require.NoError(s.T(), err)
				require.NotNil(s.T(), req)
				upgradeToWebsocket(req)
				for _, h := range tc.ProtocolHeaders {
					req.Header.Add("Sec-Websocket-Protocol", h)
				}

				// when
				resp, err := http.DefaultClient.Do(req)

				// then
				require.NoError(s.T(), err)
				require.NotNil(s.T(), resp)
				defer resp.Body.Close()
				assert.Equal(s.T(), http.StatusUnauthorized, resp.StatusCode)
				s.assertResponseBody(resp, tc.ExpectedError)
			})
		}
	})
}

func (s *TestProxySuite) checkWebLogin() {
	s.Run("web login", func() {
		// use a mock sso server
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			switch p := r.URL.Path; p {
			case "/auth/realms/sandbox-dev/.well-known/openid-configuration":
				_, err := w.Write([]byte("mock SSO configuration"))
				assert.NoError(s.T(), err)
			case "/auth/anything":
				_, err := w.Write([]byte("mock auth"))
				assert.NoError(s.T(), err)
			default:
				_, err := w.Write([]byte("unknown"))
				assert.NoError(s.T(), err)
			}
		}))
		defer testServer.Close()

		ssoBaseURL := s.DefaultConfig().Auth().SSOBaseURL()
		defer s.SetConfig(testconfig.RegistrationService().Auth().SSOBaseURL(ssoBaseURL))
		s.SetConfig(testconfig.RegistrationService().Auth().SSOBaseURL(testServer.URL))

		tests := map[string]struct {
			RequestURL         string
			ExpectedStatusCode int
			ExpectedHeaders    map[string]string
			ExpectedResponse   string
		}{
			"well-known configuration request": {
				RequestURL:         "http://localhost:8081/.well-known/oauth-authorization-server",
				ExpectedStatusCode: http.StatusOK,
				ExpectedResponse:   "mock SSO configuration",
			},
			"oidc": {
				RequestURL:         "http://localhost:8081/auth/realms/sandbox-dev/protocol/openid-connect/auth?state=mystate&code=mycode",
				ExpectedStatusCode: http.StatusSeeOther,
				ExpectedHeaders: map[string]string{
					"Location": testServer.URL + "/auth/realms/sandbox-dev/protocol/openid-connect/auth?state=mystate&code=mycode",
				},
			},
			"other auth requests": {
				RequestURL:         "http://localhost:8081/auth/anything",
				ExpectedStatusCode: http.StatusOK,
				ExpectedResponse:   "mock auth",
			},
		}
		for k, tc := range tests {
			s.Run(k, func() {
				client := &http.Client{
					CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
						return http.ErrUseLastResponse // Do not follow redirects, so we can check the actual response from the proxy
					}}

				// when
				resp, err := client.Get(tc.RequestURL)

				// then
				require.NoError(s.T(), err)
				require.NotNil(s.T(), resp)
				defer resp.Body.Close()
				assert.Equal(s.T(), tc.ExpectedStatusCode, resp.StatusCode)
				if tc.ExpectedResponse != "" {
					s.assertResponseBody(resp, tc.ExpectedResponse)
				}
				if len(tc.ExpectedHeaders) > 0 {
					for h, v := range tc.ExpectedHeaders {
						assert.Equal(s.T(), v, resp.Header.Get(h))
					}
				}
			})
		}
	})
}

func (s *TestProxySuite) checkProxyOK(proxy *Proxy) {
	s.Run("successfully proxy", func() {
		username := "smith2"

		encodedSAToken := base64.RawURLEncoding.EncodeToString([]byte("clusterSAToken"))
		encodedSSOToken := base64.RawURLEncoding.EncodeToString([]byte(s.token(username)))

		// Start the member-2 API Server
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// Set the Access-Control-Allow-Origin header to make sure it's overridden by the proxy response modifier
			w.Header().Set("Access-Control-Allow-Origin", "dummy")
			w.WriteHeader(http.StatusOK)
			_, err := w.Write([]byte("my response"))
			assert.NoError(s.T(), err)
		}))
		defer testServer.Close()

		tests := map[string]struct {
			ProxyRequestMethod              string
			ProxyRequestPaths               map[string]string
			ProxyRequestHeaders             http.Header
			ExpectedAPIServerRequestHeaders http.Header
			ExpectedProxyResponseHeaders    http.Header
			ExpectedProxyResponseStatus     int
			Standalone                      bool // If true then the request is not expected to be forwarded to the kube api server

			OverrideGetSignupFunc func(ctx *gin.Context, username string, checkUserSignupCompleted bool) (*signup.Signup, error)
			ExpectedResponse      *string
		}{
			"plain http cors preflight request with no request method": {
				ProxyRequestMethod: "OPTIONS",
				ProxyRequestHeaders: map[string][]string{
					"Origin":           {"https://domain.com"},
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {"smith2"},
				},
				ExpectedProxyResponseHeaders: noCORSHeaders,
				ExpectedProxyResponseStatus:  http.StatusUnauthorized,
				Standalone:                   true,
			},
			"plain http cors preflight request with unknown request method": {
				ProxyRequestMethod: "OPTIONS",
				ProxyRequestHeaders: map[string][]string{
					"Origin":                        {"https://domain.com"},
					"Access-Control-Request-Method": {"UNKNOWN"},
					"Authorization":                 {"Bearer clusterSAToken"},
					"Impersonate-User":              {"smith2"},
				},
				ExpectedProxyResponseHeaders: noCORSHeaders,
				ExpectedProxyResponseStatus:  http.StatusNoContent,
				Standalone:                   true,
			},
			"plain http cors preflight request with no origin": {
				ProxyRequestMethod: "OPTIONS",
				ProxyRequestHeaders: map[string][]string{
					"Access-Control-Request-Method": {"GET"},
					"Authorization":                 {"Bearer clusterSAToken"},
					"Impersonate-User":              {"smith2"},
				},
				ExpectedProxyResponseHeaders: noCORSHeaders,
				ExpectedProxyResponseStatus:  http.StatusNoContent,
				Standalone:                   true,
			},
			"plain http cors preflight request": {
				ProxyRequestMethod: "OPTIONS",
				ProxyRequestHeaders: map[string][]string{
					"Origin":                         {"https://domain.com"},
					"Access-Control-Request-Method":  {"GET"},
					"Access-Control-Request-Headers": {"Authorization"},
					"Authorization":                  {"Bearer clusterSAToken"},
					"Impersonate-User":               {"smith2"},
				},
				ExpectedProxyResponseHeaders: map[string][]string{
					"Access-Control-Allow-Origin":      {"https://domain.com"},
					"Access-Control-Allow-Credentials": {"true"},
					"Access-Control-Allow-Headers":     {"Authorization"},
					"Access-Control-Allow-Methods":     {"PUT, PATCH, POST, GET, DELETE, OPTIONS"},
					"Vary":                             {"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"},
				},
				ExpectedProxyResponseStatus: http.StatusNoContent,
				Standalone:                  true,
			},
			"plain http cors preflight request multiple request headers": {
				ProxyRequestMethod: "OPTIONS",
				ProxyRequestHeaders: map[string][]string{
					"Origin":                         {"https://domain.com"},
					"Access-Control-Request-Method":  {"GET"},
					"Access-Control-Request-Headers": {"Authorization, content-Type, header, second-header, THIRD-HEADER, Numb3r3d-H34d3r"},
					"Authorization":                  {"Bearer clusterSAToken"},
					"Impersonate-User":               {"smith2"},
				},
				ExpectedProxyResponseHeaders: map[string][]string{
					"Access-Control-Allow-Origin":      {"https://domain.com"},
					"Access-Control-Allow-Credentials": {"true"},
					"Access-Control-Allow-Headers":     {"Authorization, Content-Type, Header, Second-Header, Third-Header, Numb3r3d-H34d3r"},
					"Access-Control-Allow-Methods":     {"PUT, PATCH, POST, GET, DELETE, OPTIONS"},
					"Vary":                             {"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"},
				},
				ExpectedProxyResponseStatus: http.StatusNoContent,
				Standalone:                  true,
			},
			"plain http actual request": {
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token(username)}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {"smith2"},
				},
				ExpectedProxyResponseHeaders: map[string][]string{
					"Access-Control-Allow-Origin":      {"*"},
					"Access-Control-Allow-Credentials": {"true"},
					"Access-Control-Expose-Headers":    {"Content-Length, Content-Encoding, Authorization"},
					"Vary":                             {"Origin"},
				},
				ExpectedProxyResponseStatus: http.StatusOK,
			},
			"proxy plain http actual request as not provisioned user": {
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token("not-provisioned")}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {"smith3"},
				},
				ExpectedResponse:            ptr("unable to get target cluster: user is not provisioned (yet)"),
				ExpectedProxyResponseStatus: http.StatusInternalServerError,
			},
			"proxy plain http actual request": {
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token(username)}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {"smith2"},
				},
				ExpectedProxyResponseHeaders: map[string][]string{
					"Access-Control-Allow-Origin":      {"*"},
					"Access-Control-Allow-Credentials": {"true"},
					"Access-Control-Expose-Headers":    {"Content-Length, Content-Encoding, Authorization"},
					"Vary":                             {"Origin"},
				},
				ExpectedProxyResponseStatus: http.StatusOK,
			},
			"plain http upgrade POST request": {
				ProxyRequestMethod: "POST",
				ProxyRequestHeaders: map[string][]string{
					"Authorization": {"Bearer " + s.token(username)},
					"Connection":    {"Upgrade"},
					"Upgrade":       {"SPDY/3.1"},
				},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {"smith2"},
					"Connection":       {"Upgrade"},
					"Upgrade":          {"SPDY/3.1"},
				},
				ExpectedProxyResponseHeaders: map[string][]string{
					"Access-Control-Allow-Origin":      {"*"},
					"Access-Control-Allow-Credentials": {"true"},
					"Access-Control-Expose-Headers":    {"Content-Length, Content-Encoding, Authorization"},
					"Vary":                             {"Origin"},
				},
				ExpectedProxyResponseStatus: http.StatusOK,
			},
			"websockets": {
				ProxyRequestMethod: "GET",
				ProxyRequestHeaders: map[string][]string{
					"Connection":             {"upgrade"},
					"Upgrade":                {"websocket"},
					"Sec-Websocket-Protocol": {fmt.Sprintf("base64url.bearer.authorization.k8s.io.%s,dummy", encodedSSOToken)},
					"Impersonate-User":       {"smith2"},
				},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Connection":             {"Upgrade"},
					"Upgrade":                {"websocket"},
					"Sec-Websocket-Protocol": {fmt.Sprintf("base64url.bearer.authorization.k8s.io.%s,dummy", encodedSAToken)},
					"Impersonate-User":       {"smith2"},
				},
				ExpectedProxyResponseHeaders: map[string][]string{
					"Access-Control-Allow-Origin":      {"*"},
					"Access-Control-Allow-Credentials": {"true"},
					"Access-Control-Expose-Headers":    {"Content-Length, Content-Encoding, Authorization"},
					"Vary":                             {"Origin"},
				},
				ExpectedProxyResponseStatus: http.StatusOK,
			},
			"error retrieving user workspaces": {
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token(username)}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization": {"Bearer clusterSAToken"},
				},
				ExpectedProxyResponseStatus: http.StatusInternalServerError,
				OverrideGetSignupFunc: func(_ *gin.Context, _ string, _ bool) (*signup.Signup, error) {
					return nil, fmt.Errorf("test error")
				},
				ExpectedResponse: ptr("unable to retrieve user workspaces: test error"),
			},
			"unauthorized if workspace not exists": {
				ProxyRequestPaths: map[string]string{
					"not existing workspace namespace": "http://localhost:8081/workspaces/not-existing-workspace/api/namespaces/not-existing-namespace/pods",
				},
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token(username)}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization": {"Bearer clusterSAToken"},
				},
				ExpectedResponse:            ptr("unable to get target cluster: access to workspace 'not-existing-workspace' is forbidden"),
				ExpectedProxyResponseStatus: http.StatusInternalServerError,
			},
			"request to namespace which does not belong to implicit workspace is still proxied OK": {
				// It's not up to the proxy to check permissions on the specific namespace.
				// The target API server will reject the request if the user does not have permissions to access the namespace.
				ProxyRequestPaths: map[string]string{
					"not existing namespace": "http://localhost:8081/api/namespaces/namespace-outside-of-workspace/pods",
				},
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token(username)}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {"smith2"},
				},
				ExpectedProxyResponseHeaders: map[string][]string{
					"Access-Control-Allow-Origin":      {"*"},
					"Access-Control-Allow-Credentials": {"true"},
					"Access-Control-Expose-Headers":    {"Content-Length, Content-Encoding, Authorization"},
					"Vary":                             {"Origin"},
				},
				ExpectedProxyResponseStatus: http.StatusOK,
			},
			"request to namespace which does not belong to explicit workspace is still proxied OK": {
				ProxyRequestPaths: map[string]string{
					"not existing namespace": "http://localhost:8081/workspaces/mycoolworkspace/api/namespaces/namespace-outside-of-workspace/pods",
				},
				ProxyRequestMethod:  "GET",
				ProxyRequestHeaders: map[string][]string{"Authorization": {"Bearer " + s.token(username)}},
				ExpectedAPIServerRequestHeaders: map[string][]string{
					"Authorization":    {"Bearer clusterSAToken"},
					"Impersonate-User": {"smith2"},
				},
				ExpectedProxyResponseHeaders: map[string][]string{
					"Access-Control-Allow-Origin":      {"*"},
					"Access-Control-Allow-Credentials": {"true"},
					"Access-Control-Expose-Headers":    {"Content-Length, Content-Encoding, Authorization"},
					"Vary":                             {"Origin"},
				},
				ExpectedProxyResponseStatus: http.StatusOK,
			},
		}

		rejectedHeaders := []headerToAdd{
			{},
			{"impersonate-user", "myvalue"},
			{"Impersonate-User", "myvalue"},
			{"Impersonate-Group", "developers"},
			{"Impersonate-gRoup", "admins"},
			{"Impersonate-Extra-dn", "cn=jane,ou=engineers,dc=example,dc=com"},
			{"Impersonate-Extra-acme.com%2Fproject", "some-project"},
			{"Impersonate-Extra-scopes", "view"},
			{"Impersonate-Extra-scopes", "development"},
			{"Impersonate-Uid", "06f6ce97-e2c5-4ab8-7ba5-7654dd08d52b"},
			{"Impersonate-New", "myvalue"},
		}

		for k, tc := range tests {
			s.Run(k, func() {
				paths := tc.ProxyRequestPaths
				if len(paths) == 0 {
					paths = map[string]string{
						"default workspace":    "http://localhost:8081/api/mycoolworkspace/pods",
						"workspace context":    "http://localhost:8081/workspaces/mycoolworkspace/api/mycoolworkspace/pods",
						"proxy plugin context": "http://localhost:8081/plugins/myplugin/workspaces/mycoolworkspace/api/mycoolworkspace/pods",
					}
				}

				for _, firstHeader := range rejectedHeaders {
					rejectedHeadersToAdd := []headerToAdd{firstHeader}
					for _, additionalHeader := range rejectedHeaders {
						rejectedHeadersToAdd = append(rejectedHeadersToAdd, additionalHeader)

						// Test each request using both the default workspace URL and a URL that uses the
						// workspace context. Both should yield the same results.
						for workspaceContext, reqPath := range paths {
							s.Run(workspaceContext, func() {
								// given
								req, err := http.NewRequest(tc.ProxyRequestMethod, reqPath, nil)
								require.NoError(s.T(), err)
								require.NotNil(s.T(), req)

								for hk, hv := range tc.ProxyRequestHeaders {
									for _, v := range hv {
										req.Header.Add(hk, v)
									}
								}
								for _, header := range rejectedHeadersToAdd {
									if header.key != "" {
										req.Header.Add(header.key, header.value)
									}
								}

								if !tc.Standalone {
									testServer.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
										w.Header().Set("Content-Type", "application/json")
										// Set the Access-Control-Allow-Origin header to make sure it's overridden by the proxy response modifier
										w.Header().Set("Access-Control-Allow-Origin", "dummy")
										w.WriteHeader(http.StatusOK)
										_, err := w.Write([]byte("my response"))
										assert.NoError(s.T(), err)
										for hk, hv := range tc.ExpectedAPIServerRequestHeaders {
											assert.Len(s.T(), r.Header.Values(hk), len(hv))
											for i := range hv {
												assert.Equal(s.T(), hv[i], r.Header.Values(hk)[i])
											}
										}
										impersonateUser := tc.ExpectedAPIServerRequestHeaders.Get("Impersonate-User")
										for _, rejectedHeader := range rejectedHeaders {
											if impersonateUser != "" && strings.ToLower(rejectedHeader.key) == "impersonate-user" { // only the expected Impersonate-User header should not be rejected
												assert.NotEqual(s.T(), rejectedHeader.value, r.Header.Get(rejectedHeader.key))
											} else {
												assert.Emptyf(s.T(), r.Header.Get(rejectedHeader.key), "The header %s should be deleted", rejectedHeader.key)
												assert.Emptyf(s.T(), r.Header.Values(rejectedHeader.key), "The header %s should be deleted", rejectedHeader.key)
											}
										}
									})
									proxy.signupService = fake.NewSignupService(
										&signup.Signup{
											Name:              "someUsername",
											APIEndpoint:       "https://api.endpoint.member-1.com:6443",
											ClusterName:       "member-1",
											CompliantUsername: "smith1",
											Username:          "smith1@",
											Status: signup.Status{
												Ready: true,
											},
										},
										&signup.Signup{
											Name:              "smith2",
											APIEndpoint:       testServer.URL,
											ClusterName:       "member-2",
											CompliantUsername: "smith2",
											Username:          "smith2@",
											Status: signup.Status{
												Ready: true,
											},
										},
									)

									proxyPlugin := &toolchainv1alpha1.ProxyPlugin{
										ObjectMeta: metav1.ObjectMeta{
											Namespace: commontest.HostOperatorNs,
											Name:      "myplugin",
										},
										Spec: toolchainv1alpha1.ProxyPluginSpec{
											OpenShiftRouteTargetEndpoint: &toolchainv1alpha1.OpenShiftRouteTarget{
												Namespace: commontest.MemberOperatorNs,
												Name:      "proxy-plugin",
											},
										},
										Status: toolchainv1alpha1.ProxyPluginStatus{},
									}
									require.NoError(s.T(), routev1.Install(scheme.Scheme))
									fakeClient := commontest.NewFakeClient(s.T(),
										fake.NewSpace("mycoolworkspace", "member-2", "smith2"),
										fake.NewSpaceBinding("mycoolworkspace-smith2", "smith2", "mycoolworkspace", "admin"),
										proxyPlugin,
										fake.NewBase1NSTemplateTier())

									proxy.Client.Client = fakeClient
									proxy.getMembersFunc = s.newMemberClustersFunc(testServer.URL)
									proxy.spaceLister = &handlers.SpaceLister{
										Client:        proxy.Client,
										GetSignupFunc: proxy.signupService.GetSignup,
										ProxyMetrics:  proxy.metrics,
									}
									if tc.OverrideGetSignupFunc != nil {
										proxy.spaceLister.GetSignupFunc = tc.OverrideGetSignupFunc
									}
								}

								// when
								client := http.Client{Timeout: 3 * time.Second}
								resp, err := client.Do(req)

								// then
								require.NoError(s.T(), err)
								require.NotNil(s.T(), resp)
								defer resp.Body.Close()
								assert.Equal(s.T(), tc.ExpectedProxyResponseStatus, resp.StatusCode)
								if tc.ExpectedResponse != nil {
									s.assertResponseBody(resp, *tc.ExpectedResponse)
								} else if !tc.Standalone {
									s.assertResponseBody(resp, "my response")
								}
								for hk, hv := range tc.ExpectedProxyResponseHeaders {
									require.Lenf(s.T(), resp.Header.Values(hk), len(hv), "Actual Header %s: %v", hk, resp.Header.Values(hk))
									for i := range hv {
										assert.Equal(s.T(), hv[i], resp.Header.Values(hk)[i])
									}
								}
							})
						}
					}
				}
			})
		}
	})
}

type headerToAdd struct {
	key, value string
}

func (s *TestProxySuite) newMemberClustersFunc(serverURL string) commoncluster.GetMemberClustersFunc {
	serverHost := serverURL
	switch {
	case strings.HasPrefix(serverURL, "http://"):
		serverHost = strings.TrimPrefix(serverURL, "http://")
	case strings.HasPrefix(serverURL, "https://"):
		serverHost = strings.TrimPrefix(serverURL, "https://")
	}

	route := &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: commontest.MemberOperatorNs,
			Name:      "proxy-plugin",
		},
		Spec: routev1.RouteSpec{
			Port: &routev1.RoutePort{TargetPort: intstr.FromString("http")},
		},
		Status: routev1.RouteStatus{
			Ingress: []routev1.RouteIngress{
				{
					Host: serverHost,
				},
			},
		},
	}
	return func(_ ...commoncluster.Condition) []*commoncluster.CachedToolchainCluster {
		return []*commoncluster.CachedToolchainCluster{
			{
				Config: &commoncluster.Config{
					Name:        "member-1",
					APIEndpoint: "https://api.endpoint.member-1.com:6443",
					RestConfig:  &rest.Config{},
				},
			},
			{
				Config: &commoncluster.Config{
					Name:              "member-2",
					APIEndpoint:       serverURL,
					OperatorNamespace: "member-operator",
					RestConfig: &rest.Config{
						BearerToken: "clusterSAToken",
					},
				},
				Client: commontest.NewFakeClient(s.T(), route),
			},
		}
	}
}

var noCORSHeaders = map[string][]string{
	"Access-Control-Allow-Origin":      {},
	"Access-Control-Allow-Credentials": {},
	"Access-Control-Allow-Headers":     {},
	"Access-Control-Allow-Methods":     {},
	"Vary":                             {},
}

func ptr[T any](t T) *T {
	return &t
}

func upgradeToWebsocket(req *http.Request) {
	req.Header.Set("Connection", "upgrade")
	req.Header.Set("Upgrade", "websocket")
}

func (s *TestProxySuite) TestSingleJoiningSlash() {
	assert.Equal(s.T(), "/", singleJoiningSlash("", ""))
	assert.Equal(s.T(), "/", singleJoiningSlash("/", "/"))
	assert.Equal(s.T(), "/api/namespace/pods", singleJoiningSlash("", "api/namespace/pods"))
	assert.Equal(s.T(), "proxy/", singleJoiningSlash("proxy", ""))
	assert.Equal(s.T(), "proxy/", singleJoiningSlash("proxy", "/"))
	assert.Equal(s.T(), "proxy/api/namespace/pods", singleJoiningSlash("proxy", "api/namespace/pods"))
	assert.Equal(s.T(), "proxy/subpath/api/namespace/pods", singleJoiningSlash("proxy/subpath", "api/namespace/pods"))
	assert.Equal(s.T(), "/proxy/subpath/api/namespace/pods/", singleJoiningSlash("/proxy/subpath/", "/api/namespace/pods/"))
}

func (s *TestProxySuite) TestGetWorkspaceContext() {
	tests := map[string]struct {
		path              string
		expectedWorkspace string
		expectedPath      string
		expectedErr       string
		expectedPlugin    string
	}{
		"valid workspace context": {
			path:              "/workspaces/myworkspace/api",
			expectedWorkspace: "myworkspace",
			expectedPath:      "/api",
			expectedErr:       "",
		},
		"invalid workspace context": {
			path:              "/workspaces/myworkspace",
			expectedWorkspace: "",
			expectedPath:      "/workspaces/myworkspace",
			expectedErr:       "workspace request path has too few segments '/workspaces/myworkspace'; expected path format: /workspaces/<workspace_name>/api/...",
		},
		"no workspace context": {
			path:              "/api/pods",
			expectedWorkspace: "",
			expectedPath:      "/api/pods",
			expectedErr:       "",
		},
		"no workspace context but plugins in kube api portion": {
			path:              "/api/plugins/something",
			expectedWorkspace: "",
			expectedPath:      "/api/plugins/something",
			expectedErr:       "",
		},
		"workspace instead of workspaces": {
			path:              "/workspace/myworkspace/api",
			expectedWorkspace: "",
			expectedPath:      "/workspace/myworkspace/api",
			expectedErr:       "",
		},
		"valid workspace context with plugin": {
			path:              "/plugins/tekton-results/workspaces/myworkspace/api",
			expectedWorkspace: "myworkspace",
			expectedPath:      "/api",
			expectedErr:       "",
			expectedPlugin:    "tekton-results",
		},
		"valid workspace context with plugin plus another plugin in kube api portion": {
			path:              "/plugins/tekton-results/workspaces/myworkspace/api/plugins/something",
			expectedWorkspace: "myworkspace",
			expectedPath:      "/api/plugins/something",
			expectedErr:       "",
			expectedPlugin:    "tekton-results",
		},
		"no specific plugin segment no trailing slash": {
			path:              "/plugins",
			expectedWorkspace: "",
			expectedPath:      "/plugins",
			expectedErr:       "",
			expectedPlugin:    "",
		},
		"no specific plugin segment with trailing slash": {
			path:              "/plugins/",
			expectedWorkspace: "",
			expectedPath:      "/plugins/",
			expectedErr:       "path \"/plugins/\" not a proxied route request",
			expectedPlugin:    "",
		},
		"plugin spec but nothing else": {
			path:              "/plugins/whatever",
			expectedWorkspace: "",
			expectedPath:      "",
			expectedErr:       "",
			expectedPlugin:    "whatever",
		},
		"valid workspace context with route": {
			path:              "/plugins/tekton-results/workspaces/myworkspace",
			expectedWorkspace: "myworkspace",
			expectedPath:      "",
			expectedErr:       "",
			expectedPlugin:    "tekton-results",
		},
		"invalid workspace context with route": {
			path:              "/plugins/tekton-results/workspaces/",
			expectedWorkspace: "",
			expectedPath:      "/workspaces/",
			expectedErr:       "workspace request path has too few segments '/workspaces/'; expected path format: /workspaces/<workspace_name>/<optional path>",
			expectedPlugin:    "",
		},
		"plugin and workspaces as the sub path": {
			path:              "/plugins/tekton-results/workspaces",
			expectedWorkspace: "",
			expectedPath:      "/workspaces",
			expectedErr:       "",
			expectedPlugin:    "tekton-results",
		},
		"no workspace context with route": {
			path:              "/plugins/tekton-results/api/pods",
			expectedWorkspace: "",
			expectedPath:      "/api/pods",
			expectedErr:       "",
			expectedPlugin:    "tekton-results",
		},
		"workspace instead of workspaces with route": {
			path:              "/plugins/tekton-results/workspace/myworkspace/api",
			expectedWorkspace: "",
			expectedPath:      "/workspace/myworkspace/api",
			expectedErr:       "",
			expectedPlugin:    "tekton-results",
		},
	}

	for k, tc := range tests {
		s.Run(k, func() {
			req := &http.Request{
				URL: &url.URL{
					Path: tc.path,
				},
			}
			proxy, workspace, err := getWorkspaceContext(req)
			if tc.expectedErr == "" {
				require.NoErrorf(s.T(), err, "failed for tc %s", k)
			} else {
				require.EqualErrorf(s.T(), err, tc.expectedErr, "failed for tc %s", k)
			}
			assert.Equalf(s.T(), tc.expectedWorkspace, workspace, "failed for tc %s", k)
			assert.Equalf(s.T(), tc.expectedPath, req.URL.Path, "failed for tc %s", k)
			assert.Equalf(s.T(), tc.expectedPlugin, proxy, "failed for tc %s", k)
		})
	}
}

func (s *TestProxySuite) TestValidateWorkspaceRequest() {
	tests := map[string]struct {
		requestedWorkspace string
		workspaces         []toolchainv1alpha1.Workspace
		expectedErr        string
	}{
		"valid workspace request": {
			requestedWorkspace: "myworkspace",
			workspaces: []toolchainv1alpha1.Workspace{{
				ObjectMeta: metav1.ObjectMeta{
					Name: "myworkspace",
				},
				Status: toolchainv1alpha1.WorkspaceStatus{
					Namespaces: []toolchainv1alpha1.SpaceNamespace{
						{Name: "ns-dev"},
						{Name: "ns-stage"},
					},
				},
			},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "otherworkspace",
					},
					Status: toolchainv1alpha1.WorkspaceStatus{
						Namespaces: []toolchainv1alpha1.SpaceNamespace{
							{Name: "ns-test"},
						},
					},
				}},
			expectedErr: "",
		},
		"valid home workspace request": {
			requestedWorkspace: "", // home workspace is default when no workspace is specified
			workspaces: []toolchainv1alpha1.Workspace{{
				ObjectMeta: metav1.ObjectMeta{
					Name: "homews",
				},
				Status: toolchainv1alpha1.WorkspaceStatus{
					Type: "home", // home workspace
					Namespaces: []toolchainv1alpha1.SpaceNamespace{
						{Name: "test-1234"},
					},
				},
			}},
			expectedErr: "",
		},
		"workspace not allowed": {
			requestedWorkspace: "notexist",
			workspaces: []toolchainv1alpha1.Workspace{{
				ObjectMeta: metav1.ObjectMeta{
					Name: "myworkspace",
				},
				Status: toolchainv1alpha1.WorkspaceStatus{
					Namespaces: []toolchainv1alpha1.SpaceNamespace{
						{Name: "ns-dev"},
					},
				},
			}},
			expectedErr: "access to workspace 'notexist' is forbidden",
		},
	}

	for k, tc := range tests {
		s.Run(k, func() {
			err := validateWorkspaceRequest(tc.requestedWorkspace, tc.workspaces...)
			if tc.expectedErr == "" {
				require.NoError(s.T(), err)
			} else {
				require.EqualError(s.T(), err, tc.expectedErr)
			}
		})
	}
}

func (s *TestProxySuite) TestGetTransport() {

	s.Run("when not prod", func() {
		for _, envName := range []testconfig.EnvName{testconfig.E2E, testconfig.Dev} {
			s.Run("env "+string(envName), func() {
				// given
				env := s.DefaultConfig().Environment()
				defer s.SetConfig(testconfig.RegistrationService().
					Environment(env))
				s.SetConfig(testconfig.RegistrationService().
					Environment(string(envName)))

				// when
				transport := getTransport(map[string][]string{})

				// then
				expectedTransport := noTimeoutDefaultTransport()
				expectedTransport.TLSClientConfig = &tls.Config{
					InsecureSkipVerify: true, // nolint:gosec
				}
				assertTransport(s.T(), expectedTransport, transport)
			})
		}
	})

	s.Run("for prod", func() {
		// given
		env := s.DefaultConfig().Environment()
		defer s.SetConfig(testconfig.RegistrationService().
			Environment(env))
		s.SetConfig(testconfig.RegistrationService().
			Environment(string(testconfig.Prod)))

		s.Run("upgrade header is set to 'SPDY/3.1'", func() {
			// when
			transport := getTransport(map[string][]string{
				"Connection": {"Upgrade"},
				"Upgrade":    {"SPDY/3.1"},
			})

			// then
			expectedTransport := noTimeoutDefaultTransport().Clone()
			expectedTransport.TLSClientConfig.NextProtos = []string{"http/1.1"}
			expectedTransport.ForceAttemptHTTP2 = false

			assertTransport(s.T(), expectedTransport, transport)
		})

		s.Run("upgrade header is set to 'websocket'", func() {
			// when
			transport := getTransport(map[string][]string{
				"Connection": {"Upgrade"},
				"Upgrade":    {"websocket"},
			})

			// then
			assertTransport(s.T(), noTimeoutDefaultTransport(), transport)
		})

		s.Run("no upgrade header is set", func() {
			// when
			transport := getTransport(map[string][]string{})

			// then
			assertTransport(s.T(), noTimeoutDefaultTransport(), transport)
		})
	})

	s.Run("default transport should be same except for DailContext", func() {
		// when
		transport := http.DefaultTransport.(interface {
			Clone() *http.Transport
		}).Clone()
		transport.DialContext = noTimeoutDialerProxy

		// then
		assertTransport(s.T(), noTimeoutDefaultTransport(), transport)
	})
}

func assertTransport(t *testing.T, expected, actual *http.Transport) {
	// we need to assert TLSClientConfig directly since it's a pointer
	assert.Equal(t, expected.TLSClientConfig, actual.TLSClientConfig)
	// and now set it to nil so the different pointer address doesn't cause failures in the last assertion
	expected.TLSClientConfig = nil
	actual.TLSClientConfig = nil

	// it's not possible to use assert.Equal for comparing functions, so let's use reflect to get the pointer
	// and then set them to nil as well
	assert.Equal(t, reflect.ValueOf(expected.DialContext).Pointer(), reflect.ValueOf(actual.DialContext).Pointer())
	expected.DialContext = nil
	actual.DialContext = nil
	assert.Equal(t, reflect.ValueOf(expected.Proxy).Pointer(), reflect.ValueOf(actual.Proxy).Pointer())
	expected.Proxy = nil
	actual.Proxy = nil

	// do final comparison of the rest of the comparable params
	assert.Equal(t, expected, actual)
}

func (s *TestProxySuite) request() *http.Request {
	req, err := http.NewRequest("GET", "http://localhost:8081/api/mycoolworkspace/pods", nil)
	require.NoError(s.T(), err)
	require.NotNil(s.T(), req)
	username := uuid.New()
	req.Header.Set("Authorization", "Bearer "+s.token(username.String()))

	return req
}

func (s *TestProxySuite) token(username string, extraClaims ...authsupport.ExtraClaim) string {
	userIdentity := &authsupport.Identity{
		ID:       uuid.New(),
		Username: username,
	}

	// if an email address is not explicitly set, someemail@comp.com is used
	extra := append([]authsupport.ExtraClaim{authsupport.WithEmailClaim("someemail@comp.com")}, extraClaims...)
	token, err := authsupport.GenerateSignedE2ETestToken(*userIdentity, extra...)
	require.NoError(s.T(), err)

	return token
}

func (s *TestProxySuite) assertResponseBody(resp *http.Response, expectedBody string) {
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(resp.Body)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), expectedBody, buf.String())
}
