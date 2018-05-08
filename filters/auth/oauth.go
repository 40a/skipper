package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/zalando/skipper/filters"
	logfilter "github.com/zalando/skipper/filters/log"
)

type roleCheckType int

const (
	checkOAuthTokeninfoAnyScopes roleCheckType = iota
	checkOAuthTokeninfoAllScopes
	checkOAuthTokeninfoAnyKV
	checkOAuthTokeninfoAllKV
	checkOAuthTokenintrospectionAnyClaims
	checkOAuthTokenintrospectionAllClaims
	checkOAuthTokenintrospectionAnyKV
	checkOAuthTokenintrospectionAllKV
	checkUnknown
)

type rejectReason string

const (
	missingBearerToken rejectReason = "missing-bearer-token"
	missingToken       rejectReason = "missing-token"
	authServiceAccess  rejectReason = "auth-service-access"
	invalidSub         rejectReason = "invalid-sub-in-token"
	inactiveToken      rejectReason = "inactive-token"
	invalidToken       rejectReason = "invalid-token"
	invalidScope       rejectReason = "invalid-scope"
	invalidClaim       rejectReason = "invalid-claim"
)

const (
	OAuthTokeninfoAnyScopeName           = "oauthTokeninfoAnyScope"
	OAuthTokeninfoAllScopeName           = "oauthTokeninfoAllScope"
	OAuthTokeninfoAnyKVName              = "oauthTokeninfoAnyKV"
	OAuthTokeninfoAllKVName              = "oauthTokeninfoAllKV"
	OAuthTokenintrospectionAnyClaimsName = "oauthTokenintrospectionAnyClaims"
	OAuthTokenintrospectionAllClaimsName = "oauthTokenintrospectionAllClaims"
	OAuthTokenintrospectionAnyKVName     = "oauthTokenintrospectionAnyKV"
	OAuthTokenintrospectionAllKVName     = "oauthTokenintrospectionAllKV"
	AuthUnknown                          = "authUnknown"

	authHeaderName               = "Authorization"
	authHeaderPrefix             = "Bearer "
	accessTokenQueryKey          = "access_token"
	scopeKey                     = "scope"
	uidKey                       = "uid"
	tokeninfoCacheKey            = "tokeninfo"
	tokenintrospectionCacheKey   = "tokenintrospection"
	tokenIntrospectionConfigPath = "/.well-known/openid-configuration"
)

type (
	authClient struct {
		url *url.URL
	}

	kv map[string]string

	tokeninfoSpec struct {
		typ          roleCheckType
		tokeninfoURL string
		authClient   *authClient
	}

	tokeninfoFilter struct {
		typ        roleCheckType
		authClient *authClient
		scopes     []string
		kv         kv
	}

	tokenIntrospectionSpec struct {
		typ              roleCheckType
		issuerURL        string
		introspectionURL string
		config           *openIDConfig
		authClient       *authClient // TODO(sszuecs): might be different
	}

	openIDConfig struct {
		Issuer                            string   `json:"issuer"`
		AuthorizationEndpoint             string   `json:"authorization_endpoint"`
		TokenEndpoint                     string   `json:"token_endpoint"`
		UserinfoEndpoint                  string   `json:"userinfo_endpoint"`
		RevocationEndpoint                string   `json:"revocation_endpoint"`
		JwksURI                           string   `json:"jwks_uri"`
		RegistrationEndpoint              string   `json:"registration_endpoint"`
		IntrospectionEndpoint             string   `json:"introspection_endpoint"`
		ResponseTypesSupported            []string `json:"response_types_supported"`
		SubjectTypesSupported             []string `json:"subject_types_supported"`
		IDTokenSigningAlgValuesSupported  []string `json:"id_token_signing_alg_values_supported"`
		TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
		ClaimsSupported                   []string `json:"claims_supported"`
		ScopesSupported                   []string `json:"scopes_supported"`
		CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	}

	tokenIntrospectionInfo map[string]interface{}

	tokenintrospectFilter struct {
		typ        roleCheckType
		authClient *authClient // TODO(sszuecs): might be different
		claims     []string
		kv         kv
	}
)

var (
	errUnsupportedClaimSpecified     = errors.New("unsupported claim specified in filter")
	errInvalidAuthorizationHeader    = errors.New("invalid authorization header")
	errInvalidToken                  = errors.New("invalid token")
	errInvalidTokenintrospectionData = errors.New("invalid tokenintrospection data")
)

func (kv kv) String() string {
	var res []string
	for k, v := range kv {
		res = append(res, k, v)
	}
	return strings.Join(res, ",")
}

func getToken(r *http.Request) (string, error) {
	if tok := r.URL.Query().Get(accessTokenQueryKey); tok != "" {
		return tok, nil
	}

	h := r.Header.Get(authHeaderName)
	if !strings.HasPrefix(h, authHeaderPrefix) {
		return "", errInvalidAuthorizationHeader
	}

	return h[len(authHeaderPrefix):], nil
}

func unauthorized(ctx filters.FilterContext, uname string, reason rejectReason, hostname string) {
	ctx.StateBag()[logfilter.AuthUserKey] = uname
	ctx.StateBag()[logfilter.AuthRejectReasonKey] = string(reason)
	rsp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     make(map[string][]string),
	}
	// https://www.w3.org/Protocols/rfc2616/rfc2616-sec10.html#sec10.4.2
	rsp.Header.Add("WWW-Authenticate", hostname)
	ctx.Serve(rsp)
}

func authorized(ctx filters.FilterContext, uname string) {
	ctx.StateBag()[logfilter.AuthUserKey] = uname
}

func getStrings(args []interface{}) ([]string, error) {
	s := make([]string, len(args))
	var ok bool
	for i, a := range args {
		s[i], ok = a.(string)
		if !ok {
			return nil, filters.ErrInvalidFilterParameters
		}
	}

	return s, nil
}

// all checks that all strings in the left are also in the
// right. Right can be a superset of left.
func all(left, right []string) bool {
	for _, l := range left {
		var found bool
		for _, r := range right {
			if l == r {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// intersect checks that one string in the left is also in the right
func intersect(left, right []string) bool {
	for _, l := range left {
		for _, r := range right {
			if l == r {
				return true
			}
		}
	}

	return false
}

// jsonGet requests url with access token in the URL query specified
// by accessTokenQueryKey, if auth was given and writes into doc.
func jsonGet(url *url.URL, auth string, doc interface{}) error {
	if auth != "" {
		q := url.Query()
		q.Add(accessTokenQueryKey, auth)
		url.RawQuery = q.Encode()
	}

	req, err := http.NewRequest("GET", url.String(), nil)
	if err != nil {
		return err
	}

	rsp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	defer rsp.Body.Close()
	if rsp.StatusCode != 200 {
		return errInvalidToken
	}

	d := json.NewDecoder(rsp.Body)
	return d.Decode(doc)
}

// jsonPost requests url with access token in the body, if auth was given and writes into doc.
func jsonPost(u *url.URL, auth string, doc *tokenIntrospectionInfo) error {
	body := url.Values{}
	body.Add("token", auth)

	rsp, err := http.PostForm(u.String(), body)
	if err != nil {
		return err
	}

	defer rsp.Body.Close()
	if rsp.StatusCode != 200 {
		return errInvalidToken
	}
	buf := make([]byte, rsp.ContentLength)
	n, _ := rsp.Body.Read(buf)
	if int64(n) != rsp.ContentLength {
		log.Infof("content-length missmatch body read %d != %d", rsp.ContentLength, n)
	}
	err = json.Unmarshal(buf, &doc)
	if err != nil {
		log.Infof("Failed to unmarshal data: %v", err)
		return err
	}
	return err
}

func newAuthClient(baseURL string) (*authClient, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	return &authClient{url: u}, nil
}

func (ac *authClient) getTokeninfo(token string) (map[string]interface{}, error) {
	var a map[string]interface{}
	err := jsonGet(ac.url, token, &a)
	return a, err
}

func (ac *authClient) getTokenintrospect(token string) (tokenIntrospectionInfo, error) {
	info := make(tokenIntrospectionInfo)
	err := jsonPost(ac.url, token, &info)
	if err != nil {
		return nil, err
	}
	return info, err
}

// Active returns token introspection response, which is true if token
// is not revoked and in the time frame of
// validity. https://tools.ietf.org/html/rfc7662#section-2.2
func (tii tokenIntrospectionInfo) Active() bool {
	return tii.getBoolValue("active")
}

func (tii tokenIntrospectionInfo) AuthTime() (time.Time, error) {
	return tii.getUNIXTimeValue("auth_time")
}

func (tii tokenIntrospectionInfo) Azp() (string, error) {
	return tii.getStringValue("azp")
}

func (tii tokenIntrospectionInfo) Exp() (time.Time, error) {
	return tii.getUNIXTimeValue("exp")
}

func (tii tokenIntrospectionInfo) Iat() (time.Time, error) {
	return tii.getUNIXTimeValue("iat")
}

func (tii tokenIntrospectionInfo) Issuer() (string, error) {
	return tii.getStringValue("iss")
}

func (tii tokenIntrospectionInfo) Sub() (string, error) {
	return tii.getStringValue("sub")
}

func (tii tokenIntrospectionInfo) getBoolValue(k string) bool {
	if active, ok := tii[k].(bool); ok {
		return active
	}
	return false
}

func (tii tokenIntrospectionInfo) getStringValue(k string) (string, error) {
	s, ok := tii[k].(string)
	if !ok {
		return "", errInvalidTokenintrospectionData
	}
	return s, nil
}

func (tii tokenIntrospectionInfo) getUNIXTimeValue(k string) (time.Time, error) {
	ts, ok := tii[k].(string)
	if !ok {
		return time.Time{}, errInvalidTokenintrospectionData
	}
	ti, err := strconv.Atoi(ts)
	if err != nil {
		return time.Time{}, errInvalidTokenintrospectionData
	}

	return time.Unix(int64(ti), 0), nil
}

// NewOAuthTokeninfoAllScope creates a new auth filter specification
// to validate authorization for requests. Current implementation uses
// Bearer tokens to authorize requests and checks that the token
// contains all scopes.
func NewOAuthTokeninfoAllScope(OAuthTokeninfoURL string) filters.Spec {
	return &tokeninfoSpec{typ: checkOAuthTokeninfoAllScopes, tokeninfoURL: OAuthTokeninfoURL}
}

// NewOAuthTokeninfoAnyScope creates a new auth filter specification
// to validate authorization for requests. Current implementation uses
// Bearer tokens to authorize requests and checks that the token
// contains at least one scope.
func NewOAuthTokeninfoAnyScope(OAuthTokeninfoURL string) filters.Spec {
	return &tokeninfoSpec{typ: checkOAuthTokeninfoAnyScopes, tokeninfoURL: OAuthTokeninfoURL}
}

// NewOAuthTokeninfoAllKV creates a new auth filter specification
// to validate authorization for requests. Current implementation uses
// Bearer tokens to authorize requests and checks that the token
// contains all key value pairs provided.
func NewOAuthTokeninfoAllKV(OAuthTokeninfoURL string) filters.Spec {
	return &tokeninfoSpec{typ: checkOAuthTokeninfoAllKV, tokeninfoURL: OAuthTokeninfoURL}
}

// NewOAuthTokeninfoAnyKV creates a new auth filter specification
// to validate authorization for requests. Current implementation uses
// Bearer tokens to authorize requests and checks that the token
// contains at least one key value pair provided.
func NewOAuthTokeninfoAnyKV(OAuthTokeninfoURL string) filters.Spec {
	return &tokeninfoSpec{typ: checkOAuthTokeninfoAnyKV, tokeninfoURL: OAuthTokeninfoURL}
}

func (s *tokeninfoSpec) Name() string {
	switch s.typ {
	case checkOAuthTokeninfoAnyScopes:
		return OAuthTokeninfoAnyScopeName
	case checkOAuthTokeninfoAllScopes:
		return OAuthTokeninfoAllScopeName
	case checkOAuthTokeninfoAnyKV:
		return OAuthTokeninfoAnyKVName
	case checkOAuthTokeninfoAllKV:
		return OAuthTokeninfoAllKVName
	}
	return AuthUnknown
}

// CreateFilter creates an auth filter. All arguments have to be
// strings. Depending on the variant of the auth tokeninfoFilter, the arguments
// represent scopes or key-value pairs to be checked in the tokeninfo
// response. How scopes or key value pairs are checked is based on the
// type. The shown example for checkOAuthTokeninfoAllScopes will grant
// access only to tokens, that have scopes read-x and write-y:
//
//     s.CreateFilter(read-x", "write-y")
//
func (s *tokeninfoSpec) CreateFilter(args []interface{}) (filters.Filter, error) {
	sargs, err := getStrings(args)
	if err != nil {
		return nil, err
	}
	if len(sargs) == 0 {
		return nil, filters.ErrInvalidFilterParameters
	}

	ac, err := newAuthClient(s.tokeninfoURL)
	if err != nil {
		return nil, filters.ErrInvalidFilterParameters
	}

	f := &tokeninfoFilter{typ: s.typ, authClient: ac, kv: make(map[string]string)}
	switch f.typ {
	// all scopes
	case checkOAuthTokeninfoAllScopes:
		fallthrough
	case checkOAuthTokeninfoAnyScopes:
		f.scopes = sargs[:]
	// key value pairs
	case checkOAuthTokeninfoAnyKV:
		fallthrough
	case checkOAuthTokeninfoAllKV:
		for i := 0; i+1 < len(sargs); i += 2 {
			f.kv[sargs[i]] = sargs[i+1]
		}
		if len(sargs) == 0 || len(sargs)%2 != 0 {
			return nil, filters.ErrInvalidFilterParameters
		}
	default:
		return nil, filters.ErrInvalidFilterParameters
	}

	return f, nil
}

// String prints nicely the tokeninfoFilter configuration based on the
// configuration and check used.
func (f *tokeninfoFilter) String() string {
	switch f.typ {
	case checkOAuthTokeninfoAnyScopes:
		return fmt.Sprintf("%s(%s)", OAuthTokeninfoAnyScopeName, strings.Join(f.scopes, ","))
	case checkOAuthTokeninfoAllScopes:
		return fmt.Sprintf("%s(%s)", OAuthTokeninfoAllScopeName, strings.Join(f.scopes, ","))
	case checkOAuthTokeninfoAnyKV:
		return fmt.Sprintf("%s(%s)", OAuthTokeninfoAnyKVName, f.kv)
	case checkOAuthTokeninfoAllKV:
		return fmt.Sprintf("%s(%s)", OAuthTokeninfoAllKVName, f.kv)
	}
	return AuthUnknown
}

func (f *tokeninfoFilter) validateAnyScopes(h map[string]interface{}) bool {
	if len(f.scopes) == 0 {
		return true
	}

	vI, ok := h[scopeKey]
	if !ok {
		return false
	}
	v, ok := vI.([]interface{})
	if !ok {
		return false
	}
	var a []string
	for i := range v {
		s, ok := v[i].(string)
		if !ok {
			return false
		}
		a = append(a, s)
	}

	return intersect(f.scopes, a)
}

func (f *tokeninfoFilter) validateAllScopes(h map[string]interface{}) bool {
	if len(f.scopes) == 0 {
		return true
	}

	vI, ok := h[scopeKey]
	if !ok {
		return false
	}
	v, ok := vI.([]interface{})
	if !ok {
		return false
	}
	var a []string
	for i := range v {
		s, ok := v[i].(string)
		if !ok {
			return false
		}
		a = append(a, s)
	}

	return all(f.scopes, a)
}

func (f *tokeninfoFilter) validateAnyKV(h map[string]interface{}) bool {
	for k, v := range f.kv {
		if v2, ok := h[k].(string); ok {
			if v == v2 {
				return true
			}
		}
	}
	return false
}

func (f *tokeninfoFilter) validateAllKV(h map[string]interface{}) bool {
	if len(h) < len(f.kv) {
		return false
	}
	for k, v := range f.kv {
		v2, ok := h[k].(string)
		if !ok || v != v2 {
			return false
		}
	}
	return true
}

// Request handles authentication based on the defined auth type.
func (f *tokeninfoFilter) Request(ctx filters.FilterContext) {
	r := ctx.Request()

	var authMap map[string]interface{}
	authMapTemp, ok := ctx.StateBag()[tokeninfoCacheKey]
	if !ok {
		token, err := getToken(r)
		if err != nil {
			unauthorized(ctx, "", missingBearerToken, f.authClient.url.Hostname())
			return
		}
		if token == "" {
			unauthorized(ctx, "", missingBearerToken, f.authClient.url.Hostname())
			return
		}

		authMap, err = f.authClient.getTokeninfo(token)
		if err != nil {
			reason := authServiceAccess
			if err == errInvalidToken {
				reason = invalidToken
			}
			unauthorized(ctx, "", reason, f.authClient.url.Hostname())
			return
		}
	} else {
		authMap = authMapTemp.(map[string]interface{})
	}

	uid, _ := authMap[uidKey].(string) // uid can be empty string, but if not we set the who for auditlogging

	var allowed bool
	switch f.typ {
	case checkOAuthTokeninfoAnyScopes:
		allowed = f.validateAnyScopes(authMap)
	case checkOAuthTokeninfoAllScopes:
		allowed = f.validateAllScopes(authMap)
	case checkOAuthTokeninfoAnyKV:
		allowed = f.validateAnyKV(authMap)
	case checkOAuthTokeninfoAllKV:
		allowed = f.validateAllKV(authMap)
	default:
		log.Errorf("Wrong tokeninfoFilter type: %s", f)
	}

	if !allowed {
		unauthorized(ctx, uid, invalidScope, f.authClient.url.Hostname())
	} else {
		authorized(ctx, uid)
	}
	ctx.StateBag()[tokeninfoCacheKey] = authMap
}

func (f *tokeninfoFilter) Response(filters.FilterContext) {}

// NewOAuthTokenintrospectionAnyKV creates a new auth filter specification
// to validate authorization for requests. Current implementation uses
// Bearer tokens to authorize requests and checks that the token
// contains at least one key value pair provided.
//
// This is implementing RFC 7662 compliant implementation. It uses
// POST requests to call introspection_endpoint to get the information
// of the token validity.
//
// It uses /.well-known/openid-configuration path to the passed
// oauthIssuerURL to find introspection_endpoint as defined in draft
// https://tools.ietf.org/html/draft-ietf-oauth-discovery-06, if
// oauthIntrospectionURL is a non empty string, it will set
// IntrospectionEndpoint to the given oauthIntrospectionURL.
func NewOAuthTokenintrospectionAnyKV(oauthIssuerURL, oauthIntrospectionURL string) filters.Spec {
	return newOAuthTokenintrospectionFilter(checkOAuthTokenintrospectionAnyKV, oauthIssuerURL, oauthIntrospectionURL)
}

// NewOAuthTokenintrospectionAllKV creates a new auth filter specification
// to validate authorization for requests. Current implementation uses
// Bearer tokens to authorize requests and checks that the token
// contains at least one key value pair provided.
//
// This is implementing RFC 7662 compliant implementation. It uses
// POST requests to call introspection_endpoint to get the information
// of the token validity.
//
// It uses /.well-known/openid-configuration path to the passed
// oauthIssuerURL to find introspection_endpoint as defined in draft
// https://tools.ietf.org/html/draft-ietf-oauth-discovery-06, if
// oauthIntrospectionURL is a non empty string, it will set
// IntrospectionEndpoint to the given oauthIntrospectionURL.
func NewOAuthTokenintrospectionAllKV(oauthIssuerURL, oauthIntrospectionURL string) filters.Spec {
	return newOAuthTokenintrospectionFilter(checkOAuthTokenintrospectionAllKV, oauthIssuerURL, oauthIntrospectionURL)
}

func NewOAuthTokenintrospectionAnyClaims(oauthIssuerURL, oauthIntrospectionURL string) filters.Spec {
	return newOAuthTokenintrospectionFilter(checkOAuthTokenintrospectionAnyClaims, oauthIssuerURL, oauthIntrospectionURL)
}
func NewOAuthTokenintrospectionAllClaims(oauthIssuerURL, oauthIntrospectionURL string) filters.Spec {
	return newOAuthTokenintrospectionFilter(checkOAuthTokenintrospectionAllClaims, oauthIssuerURL, oauthIntrospectionURL)
}

func newOAuthTokenintrospectionFilter(typ roleCheckType, oauthIssuerURL, oauthIntrospectionURL string) filters.Spec {
	cfg, err := getOpenIDConfig(oauthIssuerURL)
	if err != nil {
		return &tokenIntrospectionSpec{
			typ:              typ,
			issuerURL:        oauthIssuerURL,
			introspectionURL: oauthIntrospectionURL,
		}
	}

	if oauthIntrospectionURL != "" {
		cfg.IntrospectionEndpoint = oauthIntrospectionURL
	}
	return &tokenIntrospectionSpec{
		typ:              typ,
		issuerURL:        oauthIssuerURL,
		introspectionURL: cfg.IntrospectionEndpoint,
		config:           cfg,
	}
}

func getOpenIDConfig(issuerURL string) (*openIDConfig, error) {
	u, err := url.Parse(issuerURL + tokenIntrospectionConfigPath)
	if err != nil {
		return nil, err
	}

	var cfg openIDConfig
	err = jsonGet(u, "", &cfg)
	return &cfg, err
}

func (s *tokenIntrospectionSpec) Name() string {
	switch s.typ {
	case checkOAuthTokenintrospectionAnyClaims:
		return OAuthTokenintrospectionAnyClaimsName
	case checkOAuthTokenintrospectionAllClaims:
		return OAuthTokenintrospectionAllClaimsName
	case checkOAuthTokenintrospectionAnyKV:
		return OAuthTokenintrospectionAnyKVName
	case checkOAuthTokenintrospectionAllKV:
		return OAuthTokenintrospectionAllKVName
	}
	return AuthUnknown
}

func (s *tokenIntrospectionSpec) CreateFilter(args []interface{}) (filters.Filter, error) {
	sargs, err := getStrings(args)
	if err != nil {
		return nil, err
	}
	if len(sargs) == 0 || s.introspectionURL == "" {
		return nil, filters.ErrInvalidFilterParameters
	}

	ac, err := newAuthClient(s.introspectionURL)
	if err != nil {
		return nil, filters.ErrInvalidFilterParameters
	}

	f := &tokenintrospectFilter{
		typ:        s.typ,
		authClient: ac,
		kv:         make(map[string]string),
	}
	switch f.typ {
	// similar to key value pairs but additionally checks claims to be supported before creating the filter
	case checkOAuthTokenintrospectionAllClaims:
		fallthrough
	case checkOAuthTokenintrospectionAnyClaims:
		f.claims = sargs[:]
		if s.config != nil && !all(f.claims, s.config.ClaimsSupported) {
			return nil, errUnsupportedClaimSpecified
		}
		fallthrough
	// key value pairs
	case checkOAuthTokenintrospectionAllKV:
		fallthrough
	case checkOAuthTokenintrospectionAnyKV:
		for i := 0; i+1 < len(sargs); i += 2 {
			f.kv[sargs[i]] = sargs[i+1]
		}
		if len(sargs) == 0 || len(sargs)%2 != 0 {
			return nil, filters.ErrInvalidFilterParameters
		}
	default:
		return nil, filters.ErrInvalidFilterParameters
	}

	return f, nil
}

// String prints nicely the tokenintrospectFilter configuration based on the
// configuration and check used.
func (f *tokenintrospectFilter) String() string {
	switch f.typ {
	case checkOAuthTokenintrospectionAnyClaims:
		return fmt.Sprintf("%s(%s)", OAuthTokenintrospectionAnyClaimsName, f.kv)
	case checkOAuthTokenintrospectionAllClaims:
		return fmt.Sprintf("%s(%s)", OAuthTokenintrospectionAllClaimsName, f.kv)
	case checkOAuthTokenintrospectionAnyKV:
		return fmt.Sprintf("%s(%s)", OAuthTokenintrospectionAnyKVName, f.kv)
	case checkOAuthTokenintrospectionAllKV:
		return fmt.Sprintf("%s(%s)", OAuthTokenintrospectionAllKVName, f.kv)
	}
	return AuthUnknown
}

func (f *tokenintrospectFilter) validateAllKV(info tokenIntrospectionInfo) bool {
	for k, v := range f.kv {
		v2, ok := info[k].(string)
		if !ok || v != v2 {
			return false
		}
	}
	return true
}

func (f *tokenintrospectFilter) validateAnyKV(info tokenIntrospectionInfo) bool {
	for k, v := range f.kv {
		v2, ok := info[k].(string)
		if ok && v == v2 {
			return true
		}
	}
	return false
}

func (f *tokenintrospectFilter) Request(ctx filters.FilterContext) {
	r := ctx.Request()

	var info tokenIntrospectionInfo
	infoTemp, ok := ctx.StateBag()[tokenintrospectionCacheKey]
	if !ok {
		token, err := getToken(r)
		if err != nil {
			unauthorized(ctx, "", missingToken, f.authClient.url.Hostname())
			return
		}
		if token == "" {
			unauthorized(ctx, "", missingToken, f.authClient.url.Hostname())
			return
		}

		info, err = f.authClient.getTokenintrospect(token)
		if err != nil {
			reason := authServiceAccess
			if err == errInvalidToken {
				reason = invalidToken
			}
			unauthorized(ctx, "", reason, f.authClient.url.Hostname())
			return
		}
	} else {
		info = infoTemp.(tokenIntrospectionInfo)
	}

	log.Debugf("info: %#v", info)

	sub, err := info.Sub()
	if err != nil {
		unauthorized(ctx, sub, invalidSub, f.authClient.url.Hostname())
	}

	if !info.Active() {
		unauthorized(ctx, sub, inactiveToken, f.authClient.url.Hostname())
	}

	var allowed bool
	switch f.typ {
	case checkOAuthTokenintrospectionAnyClaims:
		fallthrough
	case checkOAuthTokenintrospectionAnyKV:
		allowed = f.validateAnyKV(info)
	case checkOAuthTokenintrospectionAllClaims:
		fallthrough
	case checkOAuthTokenintrospectionAllKV:
		allowed = f.validateAllKV(info)
	default:
		log.Errorf("Wrong tokenintrospectionFilter type: %s", f)
	}

	if !allowed {
		unauthorized(ctx, sub, invalidClaim, f.authClient.url.Hostname())
	} else {
		authorized(ctx, sub)
	}
	ctx.StateBag()[tokeninfoCacheKey] = info
}
func (f *tokenintrospectFilter) Response(filters.FilterContext) {}
