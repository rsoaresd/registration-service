package configuration_test

import (
	"testing"

	"github.com/codeready-toolchain/api/api/v1alpha1"
	"github.com/codeready-toolchain/registration-service/pkg/configuration"
	"github.com/codeready-toolchain/registration-service/test"
	commonconfig "github.com/codeready-toolchain/toolchain-common/pkg/configuration"
	testconfig "github.com/codeready-toolchain/toolchain-common/pkg/test/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type TestConfigurationSuite struct {
	test.UnitTestSuite
}

func TestRunConfigurationSuite(t *testing.T) {
	suite.Run(t, &TestConfigurationSuite{test.UnitTestSuite{}})
}

func (s *TestConfigurationSuite) TestSegmentWriteKey() {
	s.Run("unit-test environment (default)", func() {
		require.True(s.T(), configuration.IsTestingMode())
	})

	s.Run("prod environment", func() {
		s.SetConfig(testconfig.RegistrationService().Environment(configuration.DefaultEnvironment))
		require.False(s.T(), configuration.IsTestingMode())
	})
}

func TestRegistrationService(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		// given
		cfg := commonconfig.NewToolchainConfigObjWithReset(t)

		// when
		regServiceCfg := configuration.NewRegistrationServiceConfig(cfg, map[string]map[string]string{})

		// then
		assert.Equal(t, "prod", regServiceCfg.Environment())
		assert.Equal(t, "info", regServiceCfg.LogLevel())
		assert.Empty(t, regServiceCfg.RegistrationServiceURL())
		assert.Empty(t, regServiceCfg.Analytics().SegmentWriteKey())
		assert.Empty(t, regServiceCfg.Analytics().DevSpacesSegmentWriteKey())
		assert.Equal(t, "https://sso.devsandbox.dev/auth/js/keycloak.js", regServiceCfg.Auth().AuthClientLibraryURL())
		assert.Equal(t, "application/json; charset=utf-8", regServiceCfg.Auth().AuthClientConfigContentType())
		assert.JSONEq(t, `{"realm": "sandbox-dev","auth-server-url": "https://sso.devsandbox.dev/auth","ssl-required": "none","resource": "sandbox-public","clientId": "sandbox-public","public-client": true, "confidential-port": 0}`,
			regServiceCfg.Auth().AuthClientConfigRaw())
		assert.Equal(t, "https://sso.devsandbox.dev/auth/realms/sandbox-dev/protocol/openid-connect/certs", regServiceCfg.Auth().AuthClientPublicKeysURL())
		assert.Equal(t, "https://sso.devsandbox.dev", regServiceCfg.Auth().SSOBaseURL())
		assert.Equal(t, "sandbox-dev", regServiceCfg.Auth().SSORealm())
		assert.False(t, regServiceCfg.Verification().Enabled())
		assert.Equal(t, 5, regServiceCfg.Verification().DailyLimit())
		assert.Equal(t, 3, regServiceCfg.Verification().AttemptsAllowed())
		assert.Equal(t, "Developer Sandbox for Red Hat OpenShift: Your verification code is %s", regServiceCfg.Verification().MessageTemplate())
		assert.Empty(t, regServiceCfg.Verification().ExcludedEmailDomains())
		assert.Equal(t, 5, regServiceCfg.Verification().CodeExpiresInMin())
		assert.Empty(t, regServiceCfg.Verification().TwilioAccountSID())
		assert.Empty(t, regServiceCfg.Verification().TwilioAuthToken())
		assert.Empty(t, regServiceCfg.Verification().TwilioFromNumber())
		assert.False(t, regServiceCfg.Verification().CaptchaEnabled())
		assert.Empty(t, regServiceCfg.Verification().CaptchaProjectID())
		assert.Empty(t, regServiceCfg.Verification().CaptchaSiteKey())
		assert.InDelta(t, float32(0.9), regServiceCfg.Verification().CaptchaScoreThreshold(), 0.01)
		assert.InDelta(t, float32(0), regServiceCfg.Verification().CaptchaRequiredScore(), 0.01)
		assert.True(t, regServiceCfg.Verification().CaptchaAllowLowScoreReactivation())
		assert.Empty(t, regServiceCfg.Verification().CaptchaServiceAccountFileContents())
		assert.False(t, regServiceCfg.PublicViewerEnabled())
	})
	t.Run("non-default", func(t *testing.T) {
		// given
		cfg := commonconfig.NewToolchainConfigObjWithReset(t, testconfig.RegistrationService().
			Environment("e2e-tests").
			LogLevel("debug").
			RegistrationServiceURL("www.crtregservice.com").
			Analytics().SegmentWriteKey("keyabc").
			Auth().AuthClientLibraryURL("https://sso.openshift.com/auth/js/keycloak.js").
			Auth().AuthClientConfigContentType("application/xml").
			Auth().AuthClientConfigRaw(`{"realm": "toolchain-private"}`).
			Auth().AuthClientPublicKeysURL("https://sso.openshift.com/certs").
			Auth().SSOBaseURL("https://sso.test.org").
			Auth().SSORealm("my-realm").
			Verification().Enabled(true).
			Verification().DailyLimit(15).
			Verification().AttemptsAllowed(13).
			Verification().MessageTemplate("Developer Sandbox verification code: %s").
			Verification().ExcludedEmailDomains("redhat.com,ibm.com").
			Verification().CodeExpiresInMin(151).
			Verification().AWSRegion("us-west-2").
			Verification().AWSSenderID("sandbox").
			Verification().AWSSMSType("Transactional").
			Verification().CaptchaEnabled(true).
			Verification().CaptchaProjectID("test-project").
			Verification().CaptchaSiteKey("site-key").
			Verification().CaptchaScoreThreshold("0.7").
			Verification().CaptchaRequiredScore("0.5").
			Verification().CaptchaAllowLowScoreReactivation(false).
			Verification().Secret().Ref("verification-secrets").
			TwilioAccountSID("twilio.sid").
			TwilioAuthToken("twilio.token").
			TwilioFromNumber("twilio.fromnumber").
			AWSAccessKeyID("aws.accesskeyid").
			AWSSecretAccessKey("aws.secretaccesskey").
			RecaptchaServiceAccountFile("captcha.json"))

		verificationSecretValues := make(map[string]string)
		verificationSecretValues["twilio.sid"] = "def"
		verificationSecretValues["twilio.token"] = "ghi"
		verificationSecretValues["twilio.fromnumber"] = "jkl"
		verificationSecretValues["aws.accesskeyid"] = "foo"
		verificationSecretValues["aws.secretaccesskey"] = "bar"
		verificationSecretValues["captcha.json"] = "example-content"
		secrets := make(map[string]map[string]string)
		secrets["verification-secrets"] = verificationSecretValues

		// when
		regServiceCfg := configuration.NewRegistrationServiceConfig(cfg, secrets)

		// then
		assert.Equal(t, "e2e-tests", regServiceCfg.Environment())
		assert.Equal(t, "debug", regServiceCfg.LogLevel())
		assert.Equal(t, "www.crtregservice.com", regServiceCfg.RegistrationServiceURL())
		assert.Equal(t, "keyabc", regServiceCfg.Analytics().SegmentWriteKey())
		assert.Equal(t, "https://sso.openshift.com/auth/js/keycloak.js", regServiceCfg.Auth().AuthClientLibraryURL())
		assert.Equal(t, "application/xml", regServiceCfg.Auth().AuthClientConfigContentType())
		assert.JSONEq(t, `{"realm": "toolchain-private"}`, regServiceCfg.Auth().AuthClientConfigRaw()) //using as per linter suggestion encoded-compare: use assert.JSONEq (testifylint)
		assert.Equal(t, "https://sso.openshift.com/certs", regServiceCfg.Auth().AuthClientPublicKeysURL())
		assert.Equal(t, "https://sso.test.org", regServiceCfg.Auth().SSOBaseURL())
		assert.Equal(t, "my-realm", regServiceCfg.Auth().SSORealm())

		assert.True(t, regServiceCfg.Verification().Enabled())
		assert.Equal(t, 15, regServiceCfg.Verification().DailyLimit())
		assert.Equal(t, 13, regServiceCfg.Verification().AttemptsAllowed())
		assert.Equal(t, "us-west-2", regServiceCfg.Verification().AWSRegion())
		assert.Equal(t, "sandbox", regServiceCfg.Verification().AWSSenderID())
		assert.Equal(t, "Transactional", regServiceCfg.Verification().AWSSMSType())
		assert.Equal(t, "Developer Sandbox verification code: %s", regServiceCfg.Verification().MessageTemplate())
		assert.Equal(t, []string{"redhat.com", "ibm.com"}, regServiceCfg.Verification().ExcludedEmailDomains())
		assert.Equal(t, 151, regServiceCfg.Verification().CodeExpiresInMin())
		assert.Equal(t, "def", regServiceCfg.Verification().TwilioAccountSID())
		assert.Equal(t, "ghi", regServiceCfg.Verification().TwilioAuthToken())
		assert.Equal(t, "jkl", regServiceCfg.Verification().TwilioFromNumber())
		assert.Equal(t, "foo", regServiceCfg.Verification().AWSAccessKeyID())
		assert.Equal(t, "bar", regServiceCfg.Verification().AWSSecretAccessKey())
		assert.True(t, regServiceCfg.Verification().CaptchaEnabled())
		assert.Equal(t, "test-project", regServiceCfg.Verification().CaptchaProjectID())
		assert.Equal(t, "site-key", regServiceCfg.Verification().CaptchaSiteKey())
		assert.InDelta(t, float32(0.7), regServiceCfg.Verification().CaptchaScoreThreshold(), 0.01)
		assert.InDelta(t, float32(0.5), regServiceCfg.Verification().CaptchaRequiredScore(), 0.01)
		assert.False(t, regServiceCfg.Verification().CaptchaAllowLowScoreReactivation())
		assert.Equal(t, "example-content", regServiceCfg.Verification().CaptchaServiceAccountFileContents())
		assert.False(t, regServiceCfg.PublicViewerEnabled())
	})
}

func TestPublicViewerConfiguration(t *testing.T) {
	tt := map[string]struct {
		name               string
		expectedValue      bool
		publicViewerConfig *v1alpha1.PublicViewerConfiguration
	}{
		"public-viewer is explicitly enabled": {
			expectedValue:      true,
			publicViewerConfig: &v1alpha1.PublicViewerConfiguration{Enabled: true},
		},
		"public-viewer is explicitly disabled": {
			expectedValue:      false,
			publicViewerConfig: &v1alpha1.PublicViewerConfiguration{Enabled: false},
		},
		"public-viewer config not set, assume disabled": {
			expectedValue:      false,
			publicViewerConfig: nil,
		},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			// given
			cfg := commonconfig.NewToolchainConfigObjWithReset(t)
			cfg.Spec.Host.PublicViewerConfig = tc.publicViewerConfig
			secrets := make(map[string]map[string]string)

			// when
			regServiceCfg := configuration.NewRegistrationServiceConfig(cfg, secrets)

			// then
			assert.Equal(t, tc.expectedValue, regServiceCfg.PublicViewerEnabled())
		})
	}
}
