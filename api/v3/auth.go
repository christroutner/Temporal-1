package v3

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bobheadxi/res"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"gopkg.in/dgrijalva/jwt-go.v3"

	"github.com/RTradeLtd/Temporal/api/v3/proto/auth"
	"github.com/RTradeLtd/Temporal/eh"
	"github.com/RTradeLtd/Temporal/queue"
	"github.com/RTradeLtd/database/v2/models"
)

const (
	claimUser      = "id"
	claimChallenge = "challenge"
	claimOrigAt    = "orig_iat"
	claimExpiry    = "exp"
)

// JWTConfig denotes JWT signing configuration
type JWTConfig struct {
	Key   string
	Realm string

	Timeout     time.Duration
	SigningAlgo jwt.SigningMethod
}

// AuthService implements TemporalAuthService
type AuthService struct {
	users   userManager
	usage   usageManager
	credits creditsManager
	emails  publisher

	verifyDomain string
	jwt          JWTConfig
	dev          bool

	l *zap.SugaredLogger
}

// NewAuthService returns a new instance of the v3 authentication service
func NewAuthService(
	users userManager,
	usage usageManager,
	credits creditsManager,
	emails publisher,

	// https://<verifyDomain>/v3/verify?user=<user>&challenge=<challenge>
	verifyDomain string,
	jwt JWTConfig,
	dev bool,

	l *zap.SugaredLogger,
) *AuthService {
	return &AuthService{users, usage, credits, emails, verifyDomain, jwt, dev, l}
}

// Register returns the Temporal API status
func (a *AuthService) Register(ctx context.Context, req *auth.RegisterReq) (*auth.User, error) {
	var (
		email = req.GetEmailAddress()
		user  = req.GetCredentials().GetUsername()
		pw    = req.GetCredentials().GetPassword()
		l     = a.l.With("user", user, "email", email)
	)

	// validate email
	if err := validateEmailFormat(email); err != nil {
		return nil, grpc.Errorf(codes.InvalidArgument, err.Error())
	}

	// nothing should be empty
	if email == "" || user == "" || pw == "" {
		return nil, grpc.Errorf(codes.InvalidArgument, "email, user, and password cannot be empty")
	}

	// create account
	if _, err := a.users.NewUserAccount(user, pw, email); err != nil {
		switch err.Error() {
		case eh.DuplicateEmailError:
			return nil, grpc.Errorf(codes.InvalidArgument, eh.DuplicateEmailError)
		case eh.DuplicateUserNameError:
			return nil, grpc.Errorf(codes.InvalidArgument, eh.DuplicateUserNameError)
		default:
			l.Errorw("unexpected error occured while creating account",
				"error", err)
			return nil, grpc.Errorf(codes.Internal, eh.UserAccountCreationError)
		}
	}

	// generate a random token to validate email
	u, err := a.users.GenerateEmailVerificationToken(user)
	if err != nil {
		l.Errorw(eh.EmailTokenGenerationError, "error", err)
		return nil, grpc.Errorf(codes.Internal, eh.EmailTokenGenerationError)
	}
	// generate a jwt used to trigger email validation
	token, err := a.signChallengeToken(u.UserName, u.EmailVerificationToken)
	if err != nil {
		l.Errorw("failed to generate email verification jwt", "error", err)
		return nil, grpc.Errorf(codes.Internal, "failed to generate email verification jwt")
	}
	// format the url the user clicks to activate email
	url := fmt.Sprintf("https://%s/v3/verify?user=%s&challenge=%s", a.verifyDomain, u.UserName, token)
	// send email message to queue for processing
	if err = a.emails.PublishMessage(queue.EmailSend{
		Subject: "Temporal Email Verification",
		Content: fmt.Sprintf("please click this %s to activate temporal email functionality",
			fmt.Sprintf("<a href=\"%s\">link</a>", url)),
		ContentType: "text/html",
		UserNames:   []string{u.UserName},
		Emails:      []string{u.EmailAddress},
	}); err != nil {
		l.Errorw(eh.QueuePublishError, "error", err)
		return nil, grpc.Errorf(codes.Internal, "failed to send verification email")
	}
	l.Info("user account registered")

	// generate default usage data
	usage, err := a.usage.NewUsageEntry(u.UserName, models.Free)
	if err != nil {
		l.Errorw("unexpected error when generating user usage data",
			"error", err)
		return nil, grpc.Errorf(codes.Internal, "failed to generate usage limits for user")
	}

	// return relevant user data
	return toUser(u, usage), nil
}

// Recover facilitates account recovery
func (a *AuthService) Recover(ctx context.Context, req *auth.RecoverReq) (*auth.Empty, error) {
	if req.GetEmailAddress() == "" {
		return nil, grpc.Errorf(codes.InvalidArgument, "email cannot be empty")
	}
	user, err := a.users.FindByEmail(req.GetEmailAddress())
	if err != nil {
		return nil, grpc.Errorf(codes.NotFound, eh.UserSearchError)
	}
	if !user.EmailEnabled {
		return nil, grpc.Errorf(codes.FailedPrecondition,
			"account does not have email enabled - unfortunately for security reasons we can't assist in recovery")
	}
	var l = a.l.With("user", user.UserName)

	switch req.GetType() {
	case auth.RecoverReq_PASSWORD:
		newPW, err := a.users.ResetPassword(user.UserName)
		if err != nil {
			l.Errorw("failed to reset password",
				"error", err)
			return nil, grpc.Errorf(codes.Internal, eh.PasswordResetError)
		}
		if err := a.emails.PublishMessage(queue.EmailSend{
			Subject:     "TEMPORAL Password Reset",
			Content:     fmt.Sprintf("your password is %s", newPW),
			ContentType: "text/html",
			UserNames:   []string{user.UserName},
			Emails:      []string{user.EmailAddress},
		}); err != nil {
			l.Errorw("failed to send email to queue",
				"error", err)
			return nil, grpc.Errorf(codes.Internal, eh.QueuePublishError)
		}
		return &auth.Empty{}, nil

	case auth.RecoverReq_USERNAME:
		if err := a.emails.PublishMessage(queue.EmailSend{
			Subject:     "Temporal Username Reminder",
			Content:     fmt.Sprintf("your username is %s", user.UserName),
			ContentType: "text/html",
			UserNames:   []string{user.UserName},
			Emails:      []string{user.EmailAddress},
		}); err != nil {
			l.Errorw("failed to send email to queue",
				"error", err)
			return nil, grpc.Errorf(codes.Internal, eh.QueuePublishError)
		}
		return &auth.Empty{}, nil

	default:
		return nil, grpc.Errorf(codes.InvalidArgument, "unsupported recovery type %v",
			req.GetType())
	}
}

// Login accepts credentials and returns a token for use with further requests.
func (a *AuthService) Login(ctx context.Context, req *auth.Credentials) (*auth.Token, error) {
	var (
		user = req.GetUsername()
		pw   = req.GetPassword()
		l    = a.l.With("user", user)
	)

	// nothing should be empty
	if user == "" || pw == "" {
		return nil, grpc.Errorf(codes.InvalidArgument, "user and password cannot be empty")
	}

	// sign in user
	ok, err := a.users.SignIn(user, pw)
	if err != nil {
		l.Errorw("unexpected error when signing in", "error", err)
		return nil, grpc.Errorf(codes.Internal, eh.LoginError)
	}
	if !ok {
		return nil, grpc.Errorf(codes.Unauthenticated, "invalid credentials provided")
	}

	// generate token
	expire, token, err := a.signAPIToken(user)
	if err != nil {
		l.Errorw("unexpected error when signing token", "error", err)
		return nil, grpc.Errorf(codes.Internal, eh.LoginError)
	}

	// return token
	return &auth.Token{
		Expire: expire,
		Token:  token,
	}, nil
}

// Account returns the account associated with an authenticated request.
func (a *AuthService) Account(ctx context.Context, req *auth.Empty) (*auth.User, error) {
	user, ok := ctxGetUser(ctx)
	if !ok {
		return nil, grpc.Errorf(codes.NotFound, "could not find user associated with token")
	}
	var l = a.l.With("user", user.UserName)

	usage, err := a.usage.FindByUserName(user.UserName)
	if err != nil {
		l.Errorw("unexpected error when retrieving user usage data",
			"error", err)
		return nil, grpc.Errorf(codes.Internal, "failed to retrieve usage for user")
	}

	l.Info("account details accessed")
	return toUser(user, usage), nil
}

// Update facilitates modification of the account associated with an
// authenticated request.
func (a *AuthService) Update(ctx context.Context, req *auth.UpdateReq) (*auth.User, error) {
	user, ok := ctxGetUser(ctx)
	if !ok {
		return nil, grpc.Errorf(codes.NotFound, "could not find user associated with token")
	}
	var l = a.l.With("user", user.UserName)

	switch v := req.GetUpdate().(type) {
	case *auth.UpdateReq_PasswordChange:
		l = l.With("change", "password")
		change := v.PasswordChange
		ok, err := a.users.ChangePassword(user.UserName, change.GetOldPassword(), change.GetNewPassword())
		if err != nil {
			l.Errorw("error when changing password",
				"error", err)
			return nil, grpc.Errorf(codes.Internal, eh.PasswordChangeError)
		}
		if !ok {
			return nil, grpc.Errorf(codes.PermissionDenied, "invalid password")
		}

		return toUser(user, nil), nil

	case *auth.UpdateReq_DataTierChange:
		l = l.With("change", "tier")
		usage, err := a.usage.FindByUserName(user.UserName)
		if err != nil {
			l.Errorw("unable to find user usage",
				"error", err)
			return nil, grpc.Errorf(codes.Internal, "unable to find usage data for user")
		}
		if usage.Tier != models.Free {
			return nil, grpc.Errorf(codes.AlreadyExists, "account is already upgraded")
		}

		if err = a.usage.UpdateTier(user.UserName, models.Light); err != nil {
			l.Errorw("unable to update user tier",
				"tier", models.Light,
				"error", err)
			return nil, grpc.Errorf(codes.Internal, eh.TierUpgradeError)
		}

		if user, err = a.credits.AddCredits(user.UserName, 0.115); err != nil {
			l.Errorw("unable to add free credits to user",
				"error", err)
			return nil, grpc.Errorf(codes.Internal, "failed to grant free credits")
		}

		if err = a.emails.PublishMessage(queue.EmailSend{
			Subject:     "TEMPORAL Account Upgraded",
			Content:     "your account has been ugpraded to Light tier. Enjoy 11.5 cents of free credit!",
			ContentType: "text/html",
			UserNames:   []string{user.UserName},
			Emails:      []string{user.EmailAddress},
		}); err != nil {
			l.Errorw("failed to send email to queue",
				"error", err)
			return nil, grpc.Errorf(codes.Internal, eh.QueuePublishError)
		}

		l.Info("user's data tier successfully updated")
		usage.Tier = models.Light
		return toUser(user, usage), nil

	default:
		return nil, grpc.Errorf(codes.InvalidArgument, "type %v is not supported", v)
	}
}

// Refresh provides a refreshed token associated with an authenticated request.
func (a *AuthService) Refresh(ctx context.Context, req *auth.Empty) (*auth.Token, error) {
	user, ok := ctxGetUser(ctx)
	if !ok {
		return nil, grpc.Errorf(codes.NotFound, "could not find user associated with token")
	}
	var l = a.l.With("user", user.UserName)

	// sign a new token for the user
	expiry, token, err := a.signAPIToken(user.UserName)
	if err != nil {
		l.Errorw("unexpected error when signing token", "error", err)
		return nil, grpc.Errorf(codes.Internal, eh.LoginError)
	}

	return &auth.Token{
		Expire: expiry,
		Token:  token,
	}, nil
}

// newAuthInterceptors creates unary and stream interceptors that validate
// requests, for use with gRPC servers, excluding the given exceptions.
// Exceptions should be the name of the gRPC method to be excluded, for example
// "/auth.TemporalAuth/Register". To find the method name of a function, search
// for "FullMethod" in *.pb.go, for example in auth.pb.go:
//
//    func _TemporalAuth_Register_Handler(...) (interface{}, error) {
//      ...
//      info := &grpc.UnaryServerInfo{
//        Server:     srv,
//        FullMethod: "/auth.TemporalAuth/Register",
//      }
//      ...
//    }
//
func (a *AuthService) newAuthInterceptors(exceptions ...string) (
	unaryInterceptor grpc.UnaryServerInterceptor,
	streamInterceptor grpc.StreamServerInterceptor,
) {
	exclude := make(map[string]bool)
	for _, e := range exceptions {
		exclude[e] = true
	}

	// unaryInterceptor handles all incoming unary RPC requests
	unaryInterceptor = func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (r interface{}, err error) {
		if v, found := exclude[info.FullMethod]; !v && !found {
			a.l.Debugw("requested RPC is an exception - skipping authentication",
				"method", info.FullMethod)
			if ctx, err = a.validate(ctx); err != nil {
				return
			}
		}
		if handler != nil {
			return handler(ctx, req)
		}
		return
	}

	// streamInterceptor handles all incoming stream RPC requests
	streamInterceptor = func(
		srv interface{},
		stream grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) (err error) {
		if v, found := exclude[info.FullMethod]; !v && !found {
			a.l.Debugw("requested RPC is an exception - skipping authentication",
				"method", info.FullMethod)
			var ctx = stream.Context()
			if ctx, err = a.validate(ctx); err != nil {
				return
			}
			wrapped := grpc_middleware.WrapServerStream(stream)
			wrapped.WrappedContext = ctx
			stream = wrapped
		}
		if handler != nil {
			return handler(srv, stream)
		}
		return
	}

	return
}

func (a *AuthService) validate(ctx context.Context) (context.Context, error) {
	// get authorization from context
	meta, ok := metadata.FromIncomingContext(ctx)
	if !ok || meta == nil {
		return nil, grpc.Errorf(codes.Unauthenticated, "missing context metadata")
	}
	keys, ok := meta["authorization"]
	if !ok || len(keys) == 0 {
		return nil, grpc.Errorf(codes.Unauthenticated, "no key provided")
	}
	var bearerString = keys[0]

	// split out the actual token from the header.
	splitToken := strings.Split(bearerString, "Bearer ")
	if len(splitToken) < 2 {
		return nil, grpc.Errorf(codes.Unauthenticated, "invalid key")
	}
	tokenString := splitToken[1]

	// parse takes the token string and a function for looking up the key.
	var (
		err    error
		user   *models.User
		claims jwt.MapClaims
	)
	if t, err := jwt.Parse(tokenString, func(t *jwt.Token) (interface{}, error) {
		// verify the claims
		if claims, ok = t.Claims.(jwt.MapClaims); !ok {
			return nil, grpc.Errorf(codes.Unauthenticated, "invalid token claims")
		}

		// check expiry
		if err := claims.Valid(); err != nil {
			return nil, grpc.Errorf(codes.Unauthenticated, "invalid claims: %s", err.Error())
		}

		// retrieve ID
		var userID string
		if userID, ok = claims[claimUser].(string); !ok || userID == "" {
			return nil, grpc.Errorf(codes.Unauthenticated, "invalid user associated with token")
		}

		// the user should be valid
		if user, err = a.users.FindByUserName(userID); err != nil {
			return nil, grpc.Errorf(codes.Unauthenticated, "unable to find user associated with token")
		}
		return []byte(a.jwt.Key), nil
	}); err != nil {
		return nil, grpc.Errorf(codes.Unauthenticated, "invalid key: %v", err)
	} else if !t.Valid {
		return nil, grpc.Errorf(codes.Unauthenticated, "invalid token")
	}

	// set user in for retrieval context
	return ctxSetUser(ctxSetClaims(ctx, claims), user), nil
}

func (a *AuthService) signAPIToken(user string) (int64, string, error) {
	expire := time.Now().Add(a.jwt.Timeout).Unix()
	token, err := jwt.
		NewWithClaims(a.jwt.SigningAlgo, jwt.MapClaims{
			claimUser:   user,
			claimExpiry: expire,
			claimOrigAt: time.Now().Unix(),
		}).
		SignedString([]byte(a.jwt.Key))
	return expire, token, err
}

func (a *AuthService) signChallengeToken(user, challenge string) (string, error) {
	return jwt.
		NewWithClaims(a.jwt.SigningAlgo, jwt.MapClaims{
			claimUser:      user,
			claimChallenge: challenge,
			claimExpiry:    time.Now().Add(time.Hour * 24).UTC().String(),
			claimOrigAt:    time.Now().Unix(),
		}).
		SignedString([]byte(a.jwt.Key))
}

func toUser(u *models.User, usage *models.Usage) *auth.User {
	return &auth.User{
		Id:           uint64(u.ID),
		UserName:     u.UserName,
		EmailAddress: u.EmailAddress,
		Verified:     u.AccountEnabled,
		Credits:      u.Credits,

		IpfsKeys: func(k []string, v []string) map[string]string {
			m := make(map[string]string)
			for i, key := range k {
				m[key] = v[i]
			}
			return m
		}(u.IPFSKeyIDs, u.IPFSKeyNames),
		IpfsNetworks: u.IPFSNetworkNames,

		Usage: func(usage *models.Usage) *auth.User_Usage {
			if usage == nil {
				return nil
			}
			return &auth.User_Usage{
				Tier: func(t models.DataUsageTier) auth.Tier {
					switch t {
					case models.Partner:
						return auth.Tier_PARTNER
					case models.Light:
						return auth.Tier_LIGHT
					case models.Plus:
						return auth.Tier_PLUS
					default:
						return auth.Tier_FREE
					}
				}(usage.Tier),
				Data: &auth.User_Usage_Limits{
					Limit: int64(usage.MonthlyDataLimitBytes),
					Used:  int64(usage.CurrentDataUsedBytes),
				},
				IpnsRecords: &auth.User_Usage_Limits{
					Limit: usage.IPNSRecordsAllowed,
					Used:  usage.IPNSRecordsAllowed,
				},
				PubsubSent: &auth.User_Usage_Limits{
					Limit: usage.PubSubMessagesAllowed,
					Used:  usage.PubSubMessagesSent,
				},
				Keys: &auth.User_Usage_Limits{
					Limit: usage.KeysAllowed,
					Used:  usage.KeysCreated,
				},
			}
		}(usage),

		ApiAccess:   true, // TODO: is this always the case?
		AdminAccess: u.AdminAccess,
	}
}

// validateEmailFormat validates the email address using the following rules:
// * email must contain '@'
// * no usage of catch-all routing (ie, username+test@example.org, username+test1@example.org)
// * no trailing periods (ie, username.@example.org, username..........@example.org)
// * domain must be valid
func validateEmailFormat(email string) error {
	if !strings.ContainsRune(email, '@') {
		return errors.New("invalid email format")
	}

	var (
		parts   = strings.Split(email, "@")
		account = parts[0]
		host    = parts[1]
	)

	// validate the account portion of email
	if strings.ContainsRune(account, '+') {
		return errors.New("email must not contain '+'")
	}
	if strings.HasSuffix(account, ".") {
		return errors.New("email must not end with '.'")
	}

	// check the given email host is valid
	if n, err := net.LookupIP(host); err != nil || len(n) == 0 {
		return fmt.Errorf("invalid email host '%s'", host)
	}

	return nil
}

// VerificationHandler is a traditional HTTP handler for handling account verifications
func (a *AuthService) VerificationHandler(
	l *zap.SugaredLogger,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var (
			user     = r.URL.Query().Get("user")
			tokenStr = r.URL.Query().Get("token")
			l        = l.With("user", user)
		)

		if user == "" || tokenStr == "" {
			res.R(w, r, res.ErrBadRequest("parameters user, token cannot be empty"))
			return
		}

		token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
			if method, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unable to validate signing method: %v", token.Header["alg"])
			} else if method != a.jwt.SigningAlgo {
				return nil, errors.New("expect hs512 signing method")
			}
			return []byte(a.jwt.Key), nil
		})
		if err != nil {
			res.R(w, r, res.ErrUnauthorized("invalid token", "error", err))
			return
		}
		if !token.Valid {
			res.R(w, r, res.ErrUnauthorized("invalid token"))
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			res.R(w, r, res.ErrBadRequest("invalid token claims"))
			return
		}
		if v, ok := claims[claimUser].(string); !ok || v != user {
			res.R(w, r, res.ErrBadRequest("user in token does not match request"))
			return
		}

		// check expiry
		if err := claims.Valid(); err != nil {
			res.R(w, r, res.ErrBadRequest("invalid claims",
				"error", err))
			return
		}

		u, err := a.users.FindByUserName(user)
		if err != nil {
			res.R(w, r, res.ErrNotFound("user not found",
				"user", user))
			return
		}
		challenge, ok := claims[claimChallenge].(string)
		if !ok || challenge != u.EmailVerificationToken {
			res.R(w, r, res.ErrBadRequest("challenge in token is incorrect"))
			return
		}
		if _, err := a.users.ValidateEmailVerificationToken(user, challenge); err != nil {
			l.Errorw("unexpected error when validating user",
				"error", err)
			res.R(w, r, res.ErrInternalServer("unable to validate user", err))
			return
		}

		l.Info("user verified")
		res.R(w, r, res.MsgOK("user verified"))
	}
}