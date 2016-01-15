// Package jwt provides Json-Web-Token authentication for the go-json-rest framework
package jwt

import (
	"github.com/ant0ine/go-json-rest/rest"
	"github.com/dgrijalva/jwt-go"

	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// JWTMiddleware provides a Json-Web-Token authentication implementation. On failure, a 401 HTTP response
// is returned. On success, the wrapped middleware is called, and the userId is made available as
// request.Env["REMOTE_USER"].(string).
// Users can get a token by posting a json request to LoginHandler. The token then needs to be passed in
// the Authentication header. Example: Authorization:Bearer XXX_TOKEN_XXX
type JWTMiddleware struct {
	// Realm name to display to the user. Required.
	Realm string

	// signing algorithm - possible values are HS256, HS384, HS512
	// Optional, default is HS256.
	SigningAlgorithm string

	// Secret key used for signing. Required.
	Key []byte

	// Duration that a jwt token is valid. Optional, defaults to one hour.
	Timeout time.Duration

	// This field allows clients to refresh their token until MaxRefresh has passed.
	// Note that clients can refresh their token in the last moment of MaxRefresh.
	// This means that the maximum validity timespan for a token is MaxRefresh + Timeout.
	// Optional, defaults to 0 meaning not refreshable.
	MaxRefresh time.Duration

	// Callback function that should perform the authentication of the user based on userId and
	// password. Must return true on success, false on failure. Required.
	Authenticator func(userId string, password string) bool

	// Callback function that should perform the authorization of the authenticated user. Called
	// only after an authentication success. Must return true on success, false on failure.
	// Optional, default to success.
	Authorizator func(userId string, request *rest.Request) bool

	// Callback function to store a token in case you want to have it checked within Authorizator in some sort of
	// database as an additional security measure
	StoreToken func(timeout time.Duration) func(username, token string)

	// Remove token when refreshing/logging out
	RemoveToken func(userId, token string)

	// Callback function that will be called during login.
	// Using this function it is possible to add additional payload data to the webtoken.
	// The data is then made available during requests via request.Env["JWT_PAYLOAD"].
	// Note that the payload is not encrypted.
	// The attributes mentioned on jwt.io can't be used as keys for the map.
	// Optional, by default no additional data will be set.
	PayloadFunc func(userId string) map[string]interface{}

	// Function that extracts token string from whichever source
	TokenExtractor func(request *rest.Request) (string, error)

	// Name of the token header to parse
	TokenName string

	// Name of the environment variable that holds the token within the rest.Request
	TokenEnvName string

	// Functions that return the token to a client, allows customising the output, e.g. return
	// a cookie instead of json body
	LoginCallback func(tokenString string, request *rest.Request, writer rest.ResponseWriter)
	RefreshCallback func(tokenString string, request *rest.Request, writer rest.ResponseWriter)
}


// MiddlewareFunc makes JWTMiddleware implement the Middleware interface.
func (mw *JWTMiddleware) MiddlewareFunc(handler rest.HandlerFunc) rest.HandlerFunc {
	if mw.TokenName == "" {
		mw.TokenName = "Authorization"
	}
	if mw.TokenEnvName == "" {
		mw.TokenEnvName = "AUTH_TOKEN"
	}
	if mw.Realm == "" {
		log.Fatal("Realm is required")
	}
	if mw.SigningAlgorithm == "" {
		mw.SigningAlgorithm = "HS256"
	}
	if mw.Key == nil {
		log.Fatal("Key required")
	}
	if mw.Timeout == 0 {
		mw.Timeout = time.Hour
	}
	if mw.Authenticator == nil {
		log.Fatal("Authenticator is required")
	}
	if mw.TokenExtractor == nil {
		mw.TokenExtractor = defaultTokenExtractor(mw)
	}
	if mw.Authorizator == nil {
		mw.Authorizator = func(userId string, request *rest.Request) bool {
			return true
		}
	}


	if mw.LoginCallback == nil {
		mw.LoginCallback = defaultResponseCallback
	}
	if mw.RefreshCallback == nil {
		mw.RefreshCallback = defaultResponseCallback
	}

	return func(writer rest.ResponseWriter, request *rest.Request) { mw.middlewareImpl(writer, request, handler) }
}

func defaultResponseCallback(tokenString string, request *rest.Request, writer rest.ResponseWriter) {
	writer.WriteJson(resultToken{Token:tokenString})
}

func defaultTokenExtractor (mw *JWTMiddleware) func(request *rest.Request) (string, error) {
	return func(request *rest.Request) (string, error) {
		authHeader := request.Header.Get(mw.TokenName)

		if authHeader == "" {
			return "", errors.New("Auth header empty")
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if !(len(parts) == 2 && parts[0] == "Bearer") {
			return "", errors.New("Invalid auth header")
		}
		return parts[1], nil
	}
}
func (mw *JWTMiddleware) middlewareImpl(writer rest.ResponseWriter, request *rest.Request, handler rest.HandlerFunc) {
	token, err := mw.parseToken(request)

	if err != nil {
		mw.unauthorized(writer)
		return
	}

	id := token.Claims["id"].(string)

	request.Env["REMOTE_USER"] = id
	request.Env["JWT_PAYLOAD"] = token.Claims
	request.Env[mw.TokenEnvName] = token.Raw

	if !mw.Authorizator(id, request) {
		mw.unauthorized(writer)
		return
	}

	handler(writer, request)
}

// ExtractClaims allows to retrieve the payload
func ExtractClaims(request *rest.Request) map[string]interface{} {
	if request.Env["JWT_PAYLOAD"] == nil {
		emptyClaims := make(map[string]interface{})
		return emptyClaims
	}
	jwtClaims := request.Env["JWT_PAYLOAD"].(map[string]interface{})
	return jwtClaims
}

type resultToken struct {
	Token string `json:"token"`
}

type login struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoginHandler can be used by clients to get a jwt token.
// Payload needs to be json in the form of {"username": "USERNAME", "password": "PASSWORD"}.
// Reply will be of the form {"token": "TOKEN"}.
func (mw *JWTMiddleware) LoginHandler(writer rest.ResponseWriter, request *rest.Request) {
	loginVals := login{}
	err := request.DecodeJsonPayload(&loginVals)

	if err != nil {
		mw.unauthorized(writer)
		return
	}

	if !mw.Authenticator(loginVals.Username, loginVals.Password) {
		mw.unauthorized(writer)
		return
	}

	token := jwt.New(jwt.GetSigningMethod(mw.SigningAlgorithm))

	if mw.PayloadFunc != nil {
		for key, value := range mw.PayloadFunc(loginVals.Username) {
			token.Claims[key] = value
		}
	}

	token.Claims["id"] = loginVals.Username
	token.Claims["exp"] = time.Now().Add(mw.Timeout).Unix()
	if mw.MaxRefresh != 0 {
		token.Claims["orig_iat"] = time.Now().Unix()
	}
	tokenString, err := token.SignedString(mw.Key)

	if err != nil {
		mw.unauthorized(writer)
		return
	}

	if mw.StoreToken != nil {
		mw.StoreToken(mw.Timeout)(loginVals.Username, tokenString)
	}

	mw.LoginCallback(tokenString, request, writer)
}

func (mw *JWTMiddleware) parseToken(request *rest.Request) (*jwt.Token, error) {
	tokenString, err := mw.TokenExtractor(request)

	if(err != nil) {
		return nil, err
	}


	return jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if jwt.GetSigningMethod(mw.SigningAlgorithm) != token.Method {
			return nil, errors.New("Invalid signing algorithm")
		}
		return mw.Key, nil
	})
}

// RefreshHandler can be used to refresh a token. The token still needs to be valid on refresh.
// Shall be put under an endpoint that is using the JWTMiddleware.
// Reply will be of the form {"token": "TOKEN"}.
func (mw *JWTMiddleware) RefreshHandler(writer rest.ResponseWriter, request *rest.Request) {
	token, err := mw.parseToken(request)

	// Token should be valid anyway as the RefreshHandler is authed
	if err != nil {
		mw.unauthorized(writer)
		return
	}

	origIat := int64(token.Claims["orig_iat"].(float64))

	if origIat < time.Now().Add(-mw.MaxRefresh).Unix() {
		mw.unauthorized(writer)
		return
	}

	newToken := jwt.New(jwt.GetSigningMethod(mw.SigningAlgorithm))

	for key := range token.Claims {
		newToken.Claims[key] = token.Claims[key]
	}

	newToken.Claims["id"] = token.Claims["id"]
	newToken.Claims["exp"] = time.Now().Add(mw.Timeout).Unix()
	newToken.Claims["orig_iat"] = origIat
	tokenString, err := newToken.SignedString(mw.Key)

	if err != nil {
		mw.unauthorized(writer)
		return
	}

	userId := newToken.Claims["id"].(string)

	if mw.StoreToken != nil {
		mw.StoreToken(mw.Timeout)(userId, tokenString)
	}

	if mw.RemoveToken != nil {
		mw.RemoveToken(userId, token.Raw)
	}

	mw.RefreshCallback(tokenString, request, writer)
}

func (mw *JWTMiddleware) unauthorized(writer rest.ResponseWriter) {
	writer.Header().Set("WWW-Authenticate", "JWT realm="+mw.Realm)
	rest.Error(writer, "Not Authorized", http.StatusUnauthorized)
}
