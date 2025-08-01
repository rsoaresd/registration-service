package service

import (
	gocontext "context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/codeready-toolchain/registration-service/pkg/context"
	"github.com/codeready-toolchain/registration-service/pkg/namespaced"
	signuppkg "github.com/codeready-toolchain/registration-service/pkg/signup"
	signupsvc "github.com/codeready-toolchain/registration-service/pkg/signup/service"
	"github.com/codeready-toolchain/registration-service/pkg/verification/sender"
	signupcommon "github.com/codeready-toolchain/toolchain-common/pkg/usersignup"
	"sigs.k8s.io/controller-runtime/pkg/client"

	toolchainv1alpha1 "github.com/codeready-toolchain/api/api/v1alpha1"
	"github.com/codeready-toolchain/registration-service/pkg/application/service"
	"github.com/codeready-toolchain/registration-service/pkg/configuration"
	crterrors "github.com/codeready-toolchain/registration-service/pkg/errors"
	"github.com/codeready-toolchain/registration-service/pkg/log"
	"github.com/codeready-toolchain/toolchain-common/pkg/hash"
	"github.com/codeready-toolchain/toolchain-common/pkg/states"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	codeCharset = "0123456789"
	codeLength  = 6

	TimestampLayout = "2006-01-02T15:04:05.000Z07:00"
)

// ServiceImpl represents the implementation of the verification service.
type ServiceImpl struct { // nolint:revive
	namespaced.Client
	HTTPClient          *http.Client
	NotificationService sender.NotificationSender
	SignupService       service.SignupService
}

type VerificationServiceOption func(svc *ServiceImpl)

// NewVerificationService creates a service object for performing user verification
func NewVerificationService(client namespaced.Client) service.VerificationService {
	httpClient := &http.Client{
		Timeout:   30*time.Second + 500*time.Millisecond, // taken from twilio code
		Transport: http.DefaultTransport,
	}
	return &ServiceImpl{
		Client:              client,
		NotificationService: sender.CreateNotificationSender(httpClient),
		SignupService:       signupsvc.NewSignupService(client),
	}
}

// InitVerification sends a verification message to the specified user, using the Twilio service.  If successful,
// the user will receive a verification SMS.  The UserSignup resource is updated with a number of annotations in order
// to manage the phone verification process and protect against system abuse.
func (s *ServiceImpl) InitVerification(ctx *gin.Context, username, e164PhoneNumber, countryCode string) error {
	signup := &toolchainv1alpha1.UserSignup{}
	if err := s.Get(gocontext.TODO(), s.NamespacedName(signupcommon.EncodeUserIdentifier(username)), signup); err != nil {
		if apierrors.IsNotFound(err) {
			log.Error(ctx, err, "usersignup not found")
			return crterrors.NewNotFoundError(err, "usersignup not found")
		}
		log.Error(ctx, err, "error retrieving usersignup")
		return crterrors.NewInternalError(err, fmt.Sprintf("error retrieving usersignup with username '%s'", username))
	}

	labelValues := map[string]string{}
	annotationValues := map[string]string{}

	// check that verification is required before proceeding
	if !states.VerificationRequired(signup) {
		log.Info(ctx, fmt.Sprintf("phone verification attempted for user without verification requirement: '%s'", signup.Name))
		return crterrors.NewBadRequest("forbidden request", "verification code will not be sent")
	}

	// Check if the provided phone number is already being used by another user
	err := PhoneNumberAlreadyInUse(s.Client, username, e164PhoneNumber)
	if err != nil {
		e := &crterrors.Error{}
		switch {
		case errors.As(err, &e) && e.Code == http.StatusForbidden:
			log.Errorf(ctx, err, "phone number already in use, cannot register using phone number: %s", e164PhoneNumber)
			return crterrors.NewForbiddenError("phone number already in use", fmt.Sprintf("cannot register using phone number: %s", e164PhoneNumber))
		default:
			log.Error(ctx, err, "error while looking up users by phone number")
			return crterrors.NewInternalError(err, "could not lookup users by phone number")
		}
	}

	// calculate the phone number hash
	phoneHash := hash.EncodeString(e164PhoneNumber)

	labelValues[toolchainv1alpha1.UserSignupUserPhoneHashLabelKey] = phoneHash

	// get the verification counter (i.e. the number of times the user has initiated phone verification within
	// the last 24 hours)
	verificationCounter := signup.Annotations[toolchainv1alpha1.UserSignupVerificationCounterAnnotationKey]
	var counter int
	cfg := configuration.GetRegistrationServiceConfig()

	dailyLimit := cfg.Verification().DailyLimit()
	if verificationCounter != "" {
		counter, err = strconv.Atoi(verificationCounter)
		if err != nil {
			// We shouldn't get an error here, but if we do, we should probably set verification counter to the daily
			// limit so that we at least now have a valid value
			log.Error(ctx, err, fmt.Sprintf("error converting annotation [%s] value [%s] to integer, on UserSignup: [%s]",
				toolchainv1alpha1.UserSignupVerificationCounterAnnotationKey,
				signup.Annotations[toolchainv1alpha1.UserSignupVerificationCounterAnnotationKey], signup.Name))
			annotationValues[toolchainv1alpha1.UserSignupVerificationCounterAnnotationKey] = strconv.Itoa(dailyLimit)
			counter = dailyLimit
		}
	}

	// read the current time
	now := time.Now()

	// If 24 hours has passed since the verification timestamp, then reset the timestamp and verification attempts
	ts, parseErr := time.Parse(TimestampLayout, signup.Annotations[toolchainv1alpha1.UserSignupVerificationInitTimestampAnnotationKey])
	if parseErr != nil || now.After(ts.Add(24*time.Hour)) {
		// Set a new timestamp
		annotationValues[toolchainv1alpha1.UserSignupVerificationInitTimestampAnnotationKey] = now.Format(TimestampLayout)
		annotationValues[toolchainv1alpha1.UserSignupVerificationCounterAnnotationKey] = "0"
		counter = 0
	}

	var initError error
	// check if counter has exceeded the limit of daily limit - if at limit error out
	if counter >= dailyLimit {
		log.Error(ctx, err, fmt.Sprintf("%d attempts made. the daily limit of %d has been exceeded", counter, dailyLimit))
		initError = crterrors.NewForbiddenError("daily limit exceeded", "cannot generate new verification code")
	}

	if initError == nil {
		// generate verification code
		verificationCode, err := generateVerificationCode()
		if err != nil {
			return crterrors.NewInternalError(err, "error while generating verification code")
		}
		// set the usersignup annotations
		annotationValues[toolchainv1alpha1.UserVerificationAttemptsAnnotationKey] = "0"
		annotationValues[toolchainv1alpha1.UserSignupVerificationCounterAnnotationKey] = strconv.Itoa(counter + 1)
		annotationValues[toolchainv1alpha1.UserSignupVerificationCodeAnnotationKey] = verificationCode
		annotationValues[toolchainv1alpha1.UserVerificationExpiryAnnotationKey] = now.Add(
			time.Duration(cfg.Verification().CodeExpiresInMin()) * time.Minute).Format(TimestampLayout)

		// Generate the verification message with the new verification code
		content := fmt.Sprintf(cfg.Verification().MessageTemplate(), verificationCode)

		err = s.NotificationService.SendNotification(ctx, content, e164PhoneNumber, countryCode)
		if err != nil {
			log.Error(ctx, err, "error while sending notification")

			// If we get an error here then just die, don't bother updating the UserSignup
			return crterrors.NewInternalError(err, "error while sending verification code")
		}
	}

	doUpdate := func() error {
		signup := &toolchainv1alpha1.UserSignup{}
		if err := s.Get(gocontext.TODO(), s.NamespacedName(signupcommon.EncodeUserIdentifier(username)), signup); err != nil {
			return err
		}
		if signup.Labels == nil {
			signup.Labels = map[string]string{}
		}

		if signup.Annotations == nil {
			signup.Annotations = map[string]string{}
		}

		for k, v := range labelValues {
			signup.Labels[k] = v
		}

		for k, v := range annotationValues {
			signup.Annotations[k] = v
		}
		if err := s.Update(gocontext.TODO(), signup); err != nil {
			return err
		}

		return nil
	}

	updateErr := signuppkg.PollUpdateSignup(ctx, doUpdate)
	if updateErr != nil {
		log.Error(ctx, updateErr, "error updating UserSignup")
		return errors.New("there was an error while updating your account - please wait a moment before " +
			"trying again. If this error persists, please contact the Developer Sandbox team at devsandbox@redhat.com for " +
			"assistance: error while verifying phone code")
	}

	return initError
}

func generateVerificationCode() (string, error) {
	buf := make([]byte, codeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}

	charsetLen := len(codeCharset)
	for i := 0; i < codeLength; i++ {
		buf[i] = codeCharset[int(buf[i])%charsetLen]
	}

	return string(buf), nil
}

// VerifyPhoneCode validates the user's phone verification code.  It updates the specified UserSignup value, so even
// if an error is returned by this function the caller should still process changes to it
func (s *ServiceImpl) VerifyPhoneCode(ctx *gin.Context, username, code string) (verificationErr error) {

	cfg := configuration.GetRegistrationServiceConfig()
	// If we can't even find the UserSignup, then die here
	signup := &toolchainv1alpha1.UserSignup{}
	if err := s.Get(gocontext.TODO(), s.NamespacedName(signupcommon.EncodeUserIdentifier(username)), signup); err != nil {
		if apierrors.IsNotFound(err) {
			log.Error(ctx, err, "usersignup not found")
			return crterrors.NewNotFoundError(err, "user not found")
		}
		log.Error(ctx, err, "error retrieving usersignup")
		return crterrors.NewInternalError(err, fmt.Sprintf("error retrieving usersignup with username '%s'", username))
	}

	// check if it's a reactivation
	if activationCounterString, foundActivationCounter := signup.Annotations[toolchainv1alpha1.UserSignupActivationCounterAnnotationKey]; foundActivationCounter && cfg.Verification().CaptchaAllowLowScoreReactivation() {
		activationCounter, err := strconv.Atoi(activationCounterString)
		if err != nil {
			log.Error(ctx, err, "activation counter is not an integer value, checking required captcha score")
			// require manual approval if captcha score below automatic verification threshold
			if err = checkRequiredManualApproval(ctx, signup, cfg); err != nil {
				return err
			}
		} else if activationCounter == 1 {
			// check required captcha score if it's not a reactivation
			if err = checkRequiredManualApproval(ctx, signup, cfg); err != nil {
				return err
			}
		}
	} else {
		// when allowLowScoreReactivation is not enabled or no activation counter found
		// require manual approval if captcha score below automatic verification threshold for all users
		if err := checkRequiredManualApproval(ctx, signup, cfg); err != nil {
			return err
		}
	}

	annotationValues := map[string]string{}
	annotationsToDelete := []string{}
	unsetVerificationRequired := false

	err := PhoneNumberAlreadyInUse(s.Client, username, signup.Labels[toolchainv1alpha1.UserSignupUserPhoneHashLabelKey])
	if err != nil {
		log.Error(ctx, err, "phone number to verify already in use")
		return crterrors.NewBadRequest("phone number already in use",
			"the phone number provided for this signup is already in use by an active account")
	}

	now := time.Now()

	attemptsMade, convErr := strconv.Atoi(signup.Annotations[toolchainv1alpha1.UserVerificationAttemptsAnnotationKey])
	if convErr != nil {
		// We shouldn't get an error here, but if we do, we will set verification attempts to max allowed
		// so that we at least now have a valid value, and let the workflow continue to the
		// subsequent attempts check
		log.Error(ctx, convErr, fmt.Sprintf("error converting annotation [%s] value [%s] to integer, on UserSignup: [%s]",
			toolchainv1alpha1.UserVerificationAttemptsAnnotationKey,
			signup.Annotations[toolchainv1alpha1.UserVerificationAttemptsAnnotationKey], signup.Name))
		attemptsMade = cfg.Verification().AttemptsAllowed()
		annotationValues[toolchainv1alpha1.UserVerificationAttemptsAnnotationKey] = strconv.Itoa(attemptsMade)
	}

	// If the user has made more attempts than is allowed per generated verification code, return an error
	if attemptsMade >= cfg.Verification().AttemptsAllowed() {
		verificationErr = crterrors.NewTooManyRequestsError("too many verification attempts", "")
	}

	if verificationErr == nil {
		// Parse the verification expiry timestamp
		exp, parseErr := time.Parse(TimestampLayout, signup.Annotations[toolchainv1alpha1.UserVerificationExpiryAnnotationKey])
		if parseErr != nil {
			// If the verification expiry timestamp is corrupt or missing, then return an error
			verificationErr = crterrors.NewInternalError(parseErr, "error parsing expiry timestamp")
		} else if now.After(exp) {
			// If it is now past the expiry timestamp for the verification code, return a 403 Forbidden error
			verificationErr = crterrors.NewForbiddenError("expired", "verification code expired")
		}
	}

	if verificationErr == nil {
		if code != signup.Annotations[toolchainv1alpha1.UserSignupVerificationCodeAnnotationKey] {
			// The code doesn't match
			attemptsMade++
			annotationValues[toolchainv1alpha1.UserVerificationAttemptsAnnotationKey] = strconv.Itoa(attemptsMade)
			verificationErr = crterrors.NewForbiddenError("invalid code", "the provided code is invalid")
		}
	}

	if verificationErr == nil {
		// If the code matches then set VerificationRequired to false, reset other verification annotations
		unsetVerificationRequired = true
		annotationsToDelete = append(annotationsToDelete, toolchainv1alpha1.UserSignupVerificationCodeAnnotationKey)
		annotationsToDelete = append(annotationsToDelete, toolchainv1alpha1.UserVerificationAttemptsAnnotationKey)
		annotationsToDelete = append(annotationsToDelete, toolchainv1alpha1.UserSignupVerificationCounterAnnotationKey)
		annotationsToDelete = append(annotationsToDelete, toolchainv1alpha1.UserSignupVerificationInitTimestampAnnotationKey)
		annotationsToDelete = append(annotationsToDelete, toolchainv1alpha1.UserVerificationExpiryAnnotationKey)
	} else {
		log.Error(ctx, verificationErr, "error validating verification code")
	}

	doUpdate := func() error {
		signup := &toolchainv1alpha1.UserSignup{}
		if err := s.Get(gocontext.TODO(), s.NamespacedName(signupcommon.EncodeUserIdentifier(username)), signup); err != nil {
			log.Error(ctx, err, fmt.Sprintf("error getting signup with username '%s'", username))
			return err
		}

		if signup.Annotations == nil {
			signup.Annotations = map[string]string{}
		}

		if unsetVerificationRequired {
			states.SetVerificationRequired(signup, false)
		}

		for k, v := range annotationValues {
			signup.Annotations[k] = v
		}

		for _, annotationName := range annotationsToDelete {
			delete(signup.Annotations, annotationName)
		}

		if err := s.Update(gocontext.TODO(), signup); err != nil {
			log.Error(ctx, err, fmt.Sprintf("error updating usersignup: %s", signup.Name))
			return err
		}

		return nil
	}

	updateErr := signuppkg.PollUpdateSignup(ctx, doUpdate)
	if updateErr != nil {
		log.Error(ctx, updateErr, "error updating UserSignup")
		return errors.New("there was an error while updating your account - please wait a moment before " +
			"trying again. If this error persists, please contact the Developer Sandbox team at devsandbox@redhat.com for " +
			"assistance: error while verifying phone code")
	}

	return
}

// checkRequiredManualApproval compares the user captcha score with the configured required captcha score.
// When the user score is lower than the required score an error is returned meaning that the user is considered "suspicious" and manual approval of the signup is required.
func checkRequiredManualApproval(ctx *gin.Context, signup *toolchainv1alpha1.UserSignup, cfg configuration.RegistrationServiceConfig) error {
	captchaScore, found := signup.Annotations[toolchainv1alpha1.UserSignupCaptchaScoreAnnotationKey]
	if found {
		fscore, parseErr := strconv.ParseFloat(captchaScore, 32)
		if parseErr != nil {
			// let's just log the parsing error and return
			log.Error(ctx, parseErr, "error while parsing captchaScore")
			return nil
		}
		if parseErr == nil && float32(fscore) < cfg.Verification().CaptchaRequiredScore() {
			log.Info(ctx, fmt.Sprintf("captcha score %v is too low, automatic verification disabled, manual approval required for user", float32(fscore)))
			return crterrors.NewForbiddenError("verification failed", "verification is not available at this time")
		}
	}
	return nil
}

// VerifyActivationCode verifies the activation code:
// - checks that the SocialEvent resource named after the activation code exists
// - checks that the SocialEvent has enough capacity to approve the user
func (s *ServiceImpl) VerifyActivationCode(ctx *gin.Context, username, code string) error {
	log.Infof(ctx, "verifying activation code '%s'", code)
	// look-up the UserSignup
	signup := &toolchainv1alpha1.UserSignup{}
	if err := s.Get(gocontext.TODO(), s.NamespacedName(signupcommon.EncodeUserIdentifier(username)), signup); err != nil {
		if apierrors.IsNotFound(err) {
			// signup user
			ctx.Set(context.SocialEvent, code)
			_, err = s.SignupService.Signup(ctx)
			return err
		}
		return crterrors.NewInternalError(err, fmt.Sprintf("error retrieving usersignup with username '%s'", username))
	}

	attemptsMade, err := checkAttempts(signup)
	if err != nil {
		return err
	}
	var errToReturn error
	doUpdate := func() error {
		signup := &toolchainv1alpha1.UserSignup{}
		if err := s.Get(gocontext.TODO(), s.NamespacedName(signupcommon.EncodeUserIdentifier(username)), signup); err != nil {
			return err
		}
		if signup.Annotations == nil {
			signup.Annotations = map[string]string{}
		}
		event, err := signuppkg.GetAndValidateSocialEvent(ctx, s.Client, code)
		if err != nil {
			attemptsMade++
			signup.Annotations[toolchainv1alpha1.UserVerificationAttemptsAnnotationKey] = strconv.Itoa(attemptsMade)
			errToReturn = err
		} else {
			log.Infof(ctx, "approving user signup request with activation code '%s'", code)
			signuppkg.UpdateUserSignupWithSocialEvent(event, signup)
			delete(signup.Annotations, toolchainv1alpha1.UserVerificationAttemptsAnnotationKey)
		}

		if err := s.Update(gocontext.TODO(), signup); err != nil {
			return err
		}

		return nil
	}
	if err := signuppkg.PollUpdateSignup(ctx, doUpdate); err != nil {
		log.Errorf(ctx, err, "unable to update user signup after validating activation code")
		if errToReturn == nil {
			errToReturn = err
		}
	}

	return errToReturn
}

var (
	md5Matcher = regexp.MustCompile("(?i)[a-f0-9]{32}$")
)

// PhoneNumberAlreadyInUse checks if the phone number has been banned. If so, return
// an internal server error. If not, check if an approved UserSignup with a different username
// and email address exists. If so, return an internal server error. Otherwise, return without error.
// Either the actual phone number, or the md5 hash of the phone number may be provided here.
func PhoneNumberAlreadyInUse(cl namespaced.Client, username, phoneNumberOrHash string) error {
	labelValue := hash.EncodeString(phoneNumberOrHash)
	if md5Matcher.Match([]byte(phoneNumberOrHash)) {
		labelValue = phoneNumberOrHash
	}

	bannedUserList := &toolchainv1alpha1.BannedUserList{}
	if err := cl.List(gocontext.TODO(), bannedUserList, client.InNamespace(cl.Namespace),
		client.MatchingLabels{toolchainv1alpha1.BannedUserPhoneNumberHashLabelKey: labelValue}); err != nil {
		return crterrors.NewInternalError(err, "failed listing banned users")
	}

	if len(bannedUserList.Items) > 0 {
		return crterrors.NewForbiddenError("cannot re-register with phone number", "phone number already in use")
	}

	labelSelector := client.MatchingLabels{
		toolchainv1alpha1.UserSignupStateLabelKey:           toolchainv1alpha1.UserSignupStateLabelValueApproved,
		toolchainv1alpha1.BannedUserPhoneNumberHashLabelKey: labelValue,
	}
	userSignups := &toolchainv1alpha1.UserSignupList{}
	if err := cl.List(gocontext.TODO(), userSignups, client.InNamespace(cl.Namespace), labelSelector); err != nil {
		return crterrors.NewInternalError(err, "failed listing userSignups")
	}

	for _, signup := range userSignups.Items {
		userSignup := signup // drop with go 1.22
		if userSignup.Spec.IdentityClaims.PreferredUsername != username && !states.Deactivated(&userSignup) {
			return crterrors.NewForbiddenError("cannot re-register with phone number",
				"phone number already in use")
		}
	}

	return nil
}

func checkAttempts(signup *toolchainv1alpha1.UserSignup) (int, error) {
	cfg := configuration.GetRegistrationServiceConfig()
	v, found := signup.Annotations[toolchainv1alpha1.UserVerificationAttemptsAnnotationKey]
	if !found || v == "" {
		return 0, nil
	}
	attemptsMade, err := strconv.Atoi(v)
	if err != nil {
		return -1, crterrors.NewInternalError(err, fmt.Sprintf("error converting annotation [%s] value [%s] to integer, on UserSignup: [%s]",
			toolchainv1alpha1.UserVerificationAttemptsAnnotationKey,
			signup.Annotations[toolchainv1alpha1.UserVerificationAttemptsAnnotationKey], signup.Name))
	}
	// If the user has made more attempts than is allowed per generated verification code, return an error
	if attemptsMade >= cfg.Verification().AttemptsAllowed() {
		return attemptsMade, crterrors.NewTooManyRequestsError("too many verification attempts", signup.Annotations[toolchainv1alpha1.UserVerificationAttemptsAnnotationKey])
	}
	return attemptsMade, nil
}
