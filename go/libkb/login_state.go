package libkb

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	keybase1 "github.com/keybase/client/protocol/go"
	jsonw "github.com/keybase/go-jsonw"
	triplesec "github.com/keybase/go-triplesec"
)

type loginReq struct {
	f    func() error
	res  chan error
	name string
}

type acctReq struct {
	f    func(*Account)
	done chan struct{}
	name string
}

type acctHandler func(*Account)

type LoginState struct {
	Contextified
	account   *Account
	loginReqs chan loginReq
	acctReqs  chan acctReq
	acctAv    chan *Account
	acctAvMu  sync.Mutex
}

type loginAPIResult struct {
	sessionID string
	csrfToken string
	uid       UID
	username  string
}

func NewLoginState(g *GlobalContext) *LoginState {
	res := &LoginState{
		Contextified: NewContextified(g),
		account:      NewAccount(g),
		loginReqs:    make(chan loginReq),
		acctReqs:     make(chan acctReq),
		acctAv:       make(chan *Account),
	}
	close(res.acctAv)
	go res.loginRequests()
	go res.acctRequests()
	return res
}

// SetSignupRes should only be called by the signup engine, and
// within an ExternalFunc handler.
func (s *LoginState) SetSignupRes(sessionID, csrfToken, username string, uid UID, salt []byte) error {
	var err error
	s.Account(func(a *Account) {
		err = a.LocalSession().Load()
		if err != nil {
			return
		}
		err = a.CreateLoginSessionWithSalt(username, salt)
	}, "SetSignupRes - LocalSession Load, CreateLoginSession")
	if err != nil {
		return err
	}

	return s.saveLoginState(&loginAPIResult{
		sessionID: sessionID,
		csrfToken: csrfToken,
		username:  username,
		uid:       uid,
	})
}

func (s *LoginState) LoginWithPrompt(username string, loginUI LoginUI, secretUI SecretUI) (err error) {
	s.G().Log.Debug("+ LoginWithPrompt(%s) called", username)
	defer func() { s.G().Log.Debug("- LoginWithPrompt -> %s", ErrToOk(err)) }()

	err = s.loginHandle(func() error {
		return s.loginWithPromptHelper(username, loginUI, secretUI, false)
	}, "loginWithPromptHelper")
	return
}

func (s *LoginState) LoginWithStoredSecret(username string) (err error) {
	s.G().Log.Debug("+ LoginWithStoredSecret(%s) called", username)
	defer func() { s.G().Log.Debug("- LoginWithStoredSecret -> %s", ErrToOk(err)) }()

	err = s.loginHandle(func() error {
		return s.loginWithStoredSecret(username)
	}, "loginWithStoredSecret")
	return
}

func (s *LoginState) LoginWithPassphrase(username, passphrase string, storeSecret bool) (err error) {
	s.G().Log.Debug("+ LoginWithPassphrase(%s) called", username)
	defer func() { s.G().Log.Debug("- LoginWithPassphrase -> %s", ErrToOk(err)) }()

	err = s.loginHandle(func() error {
		return s.loginWithPassphrase(username, passphrase, storeSecret)
	}, "loginWithPassphrase")
	return
}

func (s *LoginState) Logout() error {
	return s.loginHandle(func() error {
		return s.logout()
	}, "logout")
}

// ExternalFunc is for having the LoginState handler call a
// function outside of LoginState.  The current use case is
// for signup, so that no logins/logouts happen while a signup is
// happening.
func (s *LoginState) ExternalFunc(f func() error, name string) error {
	return s.loginHandle(f, name)
}

func (s *LoginState) Shutdown() error {
	close(s.loginReqs)
	close(s.acctReqs)
	return nil
}

// GetPassphraseStream either returns a cached, verified passphrase stream
// (maybe from a previous login) or generates a new one via Login. It will
// return the current Passphrase stream on success or an error on failure.
func (s *LoginState) GetPassphraseStream(ui SecretUI) (ret PassphraseStream, err error) {
	if ret = s.PassphraseStream(); ret != nil {
		return
	}
	if err = s.verifyPassphrase(ui); err != nil {
		return
	}
	if ret = s.PassphraseStream(); ret != nil {
		return
	}
	err = InternalError{"No cached keystream data after login attempt"}
	return
}

// GetVerifiedTripleSec either returns a cached, verified Triplesec
// or generates a new one that's verified via Login.
func (s *LoginState) GetVerifiedTriplesec(ui SecretUI) (ret *triplesec.Cipher, err error) {
	s.Account(func(a *Account) {
		ret = a.PassphraseStreamCache().Triplesec()
	}, "LoginState - GetVerifiedTriplesec - first")
	if ret != nil {
		return
	}

	if err = s.verifyPassphrase(ui); err != nil {
		return
	}

	s.Account(func(a *Account) {
		ret = a.PassphraseStreamCache().Triplesec()
	}, "LoginState - GetVerifiedTriplesec - first")
	if ret != nil {
		return
	}
	err = InternalError{"No cached keystream data after login attempt"}
	return
}

func (s *LoginState) computeLoginPw() (macSum []byte, err error) {
	s.Account(func(a *Account) {
		loginSession, e := a.LoginSession().Session()
		if e != nil {
			err = e
			return
		}
		if loginSession == nil {
			err = fmt.Errorf("nil login session")
			return
		}
		sec := a.PassphraseStreamCache().PassphraseStream().PWHash()
		mac := hmac.New(sha512.New, sec)
		mac.Write(loginSession)
		macSum = mac.Sum(nil)
	}, "LoginState - computeLoginPw")
	return
}

func (s *LoginState) postLoginToServer(eOu string, lgpw []byte) (*loginAPIResult, error) {
	var loginSessionEncoded string
	var err error
	s.LoginSession(func(ls *LoginSession) {
		loginSessionEncoded, err = ls.SessionEncoded()
	}, "LoginState - postLoginToServer")
	if err != nil {
		return nil, err
	}
	res, err := s.G().API.Post(ApiArg{
		Endpoint:    "login",
		NeedSession: false,
		Args: HttpArgs{
			"email_or_username": S{eOu},
			"hmac_pwh":          S{hex.EncodeToString(lgpw)},
			"login_session":     S{loginSessionEncoded},
		},
		AppStatus: []string{"OK", "BAD_LOGIN_PASSWORD"},
	})
	if err != nil {
		return nil, err
	}
	if res.AppStatus == "BAD_LOGIN_PASSWORD" {
		err = PassphraseError{"server rejected login attempt"}
		return nil, err
	}

	b := res.Body
	sessionId, err := b.AtKey("session").GetString()
	if err != nil {
		return nil, err
	}
	csrfToken, err := b.AtKey("csrf_token").GetString()
	if err != nil {
		return nil, err
	}
	uid, err := GetUid(b.AtKey("uid"))
	if err != nil {
		return nil, err
	}
	uname, err := b.AtKey("me").AtKey("basics").AtKey("username").GetString()
	if err != nil {
		return nil, err
	}

	return &loginAPIResult{sessionId, csrfToken, *uid, uname}, nil
}

func (s *LoginState) saveLoginState(res *loginAPIResult) error {
	cw := s.G().Env.GetConfigWriter()
	if cw == nil {
		return NoConfigWriterError{}
	}

	var err error
	s.Account(func(a *Account) {
		if err = a.LoginSession().Clear(); err != nil {
			return
		}
		var salt []byte
		salt, err = a.LoginSession().Salt()
		if err != nil {
			return
		}
		if err = cw.SetUserConfig(NewUserConfig(res.uid, res.username, salt, nil), false); err != nil {
			return
		}
		if err = cw.Write(); err != nil {
			return
		}
		a.LocalSession().SetLoggedIn(res.sessionID, res.csrfToken, res.username, res.uid)
		if err = a.LocalSession().Write(); err != nil {
			return
		}

		// Set up our SecretSyncer to work on the logged in user from here on
		// out.
		// (note: I really don't think this matters since RunSyncer(SecretSyncer, uid)
		// is always called with a uid... --PC)
		a.SecretSyncer().SetUID(&res.uid)
	}, "LoginState - saveLoginState")

	return err
}

func (r PostAuthProofRes) loginResult() (ret *loginAPIResult, err error) {
	var uid *UID
	if uid, err = UidFromHex(r.UidHex); err != nil {
		return
	}
	ret = &loginAPIResult{
		sessionID: r.SessionId,
		csrfToken: r.CsrfToken,
		uid:       *uid,
		username:  r.Username,
	}
	return
}

// A function that takes a Keyrings object, a user, and returns a
// particular key for that user.
type getSecretKeyFn func(*Keyrings, *User) (GenericKey, error)

// pubkeyLoginHelper looks for a locally available private key and
// tries to establish a session via public key signature.
func (s *LoginState) pubkeyLoginHelper(username string, getSecretKeyFn getSecretKeyFn) (err error) {
	var key GenericKey
	var me *User
	var proof *jsonw.Wrapper
	var sig string
	var pres *PostAuthProofRes

	s.G().Log.Debug("+ pubkeyLoginHelper()")
	defer func() {
		if err != nil {
			s.SecretSyncer(func(ss *SecretSyncer) {
				ss.Clear()
			}, "pubkeyLoginHelper - SecretSyncer Clear")
		}
		s.G().Log.Debug("- pubkeyLoginHelper() -> %s", ErrToOk(err))
	}()

	if _, err = s.G().Env.GetConfig().GetUserConfigForUsername(username); err != nil {
		s.G().Log.Debug("| No Userconfig for %s: %s", username, err.Error())
		return
	}

	if me, err = LoadUser(LoadUserArg{Name: username}); err != nil {
		return
	}

	var loginSessionEncoded string
	s.Account(func(a *Account) {
		// Need the loginSession; the salt doesn't really matter here.
		if err = a.LoadLoginSession(username); err != nil {
			return
		}

		loginSessionEncoded, err = a.LoginSession().SessionEncoded()
	}, "LoginState - pubkeyLoginHelper")
	if err != nil {
		return err
	}

	if key, err = getSecretKeyFn(s.G().Keyrings, me); err != nil {
		return err
	}

	if proof, err = me.AuthenticationProof(key, loginSessionEncoded, AUTH_EXPIRE_IN); err != nil {
		return
	}

	if sig, _, _, err = SignJson(proof, key); err != nil {
		return
	}

	arg := PostAuthProofArg{
		uid: me.id,
		sig: sig,
		key: key,
	}
	if pres, err = PostAuthProof(arg); err != nil {
		return
	}

	res, err := pres.loginResult()
	if err != nil {
		return err
	}

	return s.saveLoginState(res)
}

func (s *LoginState) checkLoggedIn(username string, force bool) (loggedIn bool, err error) {
	s.G().Log.Debug("+ checkedLoggedIn()")
	defer func() { s.G().Log.Debug("- checkedLoggedIn() -> %t, %s", loggedIn, ErrToOk(err)) }()

	var loggedInTmp bool
	if loggedInTmp, err = s.LoggedInLoad(); err != nil {
		s.G().Log.Debug("| Session failed to load")
		return
	}

	var un *string
	s.LocalSession(func(ls *Session) {
		un = ls.GetUsername()
	}, "checkLoggedIn - GetUsername")
	if loggedInTmp && len(username) > 0 && un != nil && username != *un {
		err = LoggedInError{}
		return
	}

	if !force && loggedInTmp {
		s.G().Log.Debug("| Our session token is still valid; we're logged in")
		loggedIn = true
	}
	return
}

func (s *LoginState) switchUser(username string) error {
	if len(username) == 0 {
		// this isn't an error
		return nil
	}
	if !CheckUsername.F(username) {
		return errors.New("invalid username provided to switchUser")
	}
	if err := s.G().Env.GetConfigWriter().SwitchUser(username); err != nil {
		s.G().Log.Debug("| Can't switch user to %s: %s", username, err)
		// apparently this isn't an error either
		return nil
	}

	s.Account(func(a *Account) {
		a.EnsureUsername(username)
	}, "LoginState - switchUser")

	s.G().Log.Debug("| Successfully switched user to %s", username)
	return nil
}

// Like pubkeyLoginHelper, but ignores most errors.
func (s *LoginState) tryPubkeyLoginHelper(username string, getSecretKeyFn getSecretKeyFn) (loggedIn bool, err error) {
	if err = s.pubkeyLoginHelper(username, getSecretKeyFn); err == nil {
		s.G().Log.Debug("| Pubkey login succeeded")
		loggedIn = true
		return
	}

	if _, ok := err.(CanceledError); ok {
		s.G().Log.Debug("| Canceled pubkey login, so cancel login")
		return
	}

	s.G().Log.Debug("| Public key login failed, falling back: %s", err.Error())
	err = nil
	return
}

func (s *LoginState) tryPassphrasePromptLogin(username string, secretUI SecretUI) (err error) {
	retryMsg := ""
	retryCount := 3
	for i := 0; i < retryCount; i++ {
		err = s.passphraseLogin(username, "", secretUI, retryMsg)

		if err == nil {
			return
		}

		if _, badpw := err.(PassphraseError); !badpw {
			return
		}

		retryMsg = err.Error()
	}
	return
}

func (s *LoginState) getEmailOrUsername(username *string, loginUI LoginUI) (err error) {
	if len(*username) != 0 {
		return
	}

	*username = s.G().Env.GetEmailOrUsername()
	if len(*username) != 0 {
		return
	}

	if loginUI != nil {
		if *username, err = loginUI.GetEmailOrUsername(0); err != nil {
			*username = ""
			return
		}
	}

	if len(*username) == 0 {
		err = NoUsernameError{}
	}

	if err != nil {
		return err
	}

	// username set, so redo config
	s.G().ConfigureConfig()
	return s.switchUser(*username)
}

func (s *LoginState) passphraseLogin(username, passphrase string, secretUI SecretUI, retryMsg string) (err error) {
	s.G().Log.Debug("+ LoginState.passphraseLogin (username=%s)", username)
	defer func() {
		s.G().Log.Debug("- LoginState.passphraseLogin -> %s", ErrToOk(err))
	}()

	s.Account(func(a *Account) {
		if err = a.LoadLoginSession(username); err != nil {
			return
		}
	}, "LoginState - passphraseLogin - LoadLoginSession")
	if err != nil {
		return
	}

	if err = s.stretchPassphraseIfNecessary(username, passphrase, secretUI, retryMsg); err != nil {
		return err
	}

	lgpw, err := s.computeLoginPw()
	if err != nil {
		return
	}

	res, err := s.postLoginToServer(username, lgpw)
	if err != nil {
		s.Account(func(a *Account) {
			a.ClearStreamCache()
		}, "LoginState - passphraseLogin - clearstreamcache")
		return err
	}

	if err := s.saveLoginState(res); err != nil {
		return err
	}

	return nil
}

func (s *LoginState) stretchPassphraseIfNecessary(un string, pp string, ui SecretUI, retry string) error {
	var cached bool
	s.PassphraseStreamCache(func(sc *PassphraseStreamCache) {
		if sc.Valid() {
			// already have stretched passphrase cached
			cached = true
		}
	}, "LoginState - stretchPassphraseIfNecessary")
	if cached {
		return nil
	}

	arg := keybase1.GetKeybasePassphraseArg{
		Username: un,
		Retry:    retry,
	}

	if len(pp) == 0 {
		if ui == nil {
			return NoUiError{"secret"}
		}

		var err error
		if pp, err = ui.GetKeybasePassphrase(arg); err != nil {
			return err
		}
	}

	var err error
	s.Account(func(a *Account) {
		err = a.CreateStreamCacheViaStretch(pp)
	}, "LoginState - stretchPPIfNec - CreateStreamCacheViaStretch")
	return err
}

func (s *LoginState) verifyPassphrase(ui SecretUI) error {
	return s.loginHandle(func() error {
		return s.loginWithPromptHelper(s.G().Env.GetUsername(), nil, ui, true)
	}, "LoginState - verifyPassphrase")
}

func (s *LoginState) loginWithPromptHelper(username string, loginUI LoginUI, secretUI SecretUI, force bool) (err error) {
	var loggedIn bool
	if loggedIn, err = s.checkLoggedIn(username, force); err != nil || loggedIn {
		return
	}

	if err = s.switchUser(username); err != nil {
		return
	}

	if err = s.getEmailOrUsername(&username, loginUI); err != nil {
		return
	}

	getSecretKeyFn := func(keyrings *Keyrings, me *User) (GenericKey, error) {
		ska := SecretKeyArg{
			Me:      me,
			KeyType: AnySecretKeyType,
		}
		key, _, err := keyrings.GetSecretKeyWithPrompt(ska, secretUI, "Login")
		return key, err
	}

	if loggedIn, err = s.tryPubkeyLoginHelper(username, getSecretKeyFn); err != nil || loggedIn {
		return
	}

	return s.tryPassphrasePromptLogin(username, secretUI)
}

func (s *LoginState) loginHandle(f func() error, name string) error {
	req := loginReq{
		f:    f,
		res:  make(chan error),
		name: name,
	}
	s.G().Log.Debug("+ send login request %q", name)
	s.loginReqs <- req
	s.G().Log.Debug("- send login request %q", name)

	s.G().Log.Debug("+ wait login request %q", name)
	err := <-req.res
	s.G().Log.Debug("- wait login request %q", name)

	return err
}

func (s *LoginState) acctHandle(f acctHandler, name string) {
	req := acctReq{
		f:    f,
		done: make(chan struct{}),
		name: name,
	}
	s.G().Log.Debug("+ send acct request %q", name)
	s.acctReqs <- req
	s.G().Log.Debug("- send acct request %q", name)

	s.G().Log.Debug("+ wait acct request %q", name)
	<-req.done
	s.G().Log.Debug("- wait acct request %q", name)

	return
}

func (s *LoginState) loginRequests() {
	for req := range s.loginReqs {
		s.G().Log.Debug("+ login request %s", req.name)
		req.res <- req.f()
		s.G().Log.Debug("- login request %s", req.name)
	}
}

func (s *LoginState) acctRequests() {
	for req := range s.acctReqs {
		s.G().Log.Debug("+ account request %s", req.name)
		req.f(s.account)
		close(req.done)
		s.G().Log.Debug("- account request %s", req.name)
	}
}

func (s *LoginState) loginWithStoredSecret(username string) error {
	if loggedIn, err := s.checkLoggedIn(username, false); err != nil {
		return err
	} else if loggedIn {
		return nil
	}

	if err := s.switchUser(username); err != nil {
		return err
	}

	getSecretKeyFn := func(keyrings *Keyrings, me *User) (GenericKey, error) {
		secretRetriever := NewSecretStore(me.GetName())
		return keyrings.GetSecretKeyWithStoredSecret(me, secretRetriever)
	}
	return s.pubkeyLoginHelper(username, getSecretKeyFn)
}

func (s *LoginState) loginWithPassphrase(username, passphrase string, storeSecret bool) error {
	if loggedIn, err := s.checkLoggedIn(username, false); err != nil {
		return err
	} else if loggedIn {
		return nil
	}

	if err := s.switchUser(username); err != nil {
		return err
	}

	getSecretKeyFn := func(keyrings *Keyrings, me *User) (GenericKey, error) {
		var secretStorer SecretStorer
		if storeSecret {
			secretStorer = NewSecretStore(me.GetName())
		}
		return keyrings.GetSecretKeyWithPassphrase(me, passphrase, secretStorer)
	}
	if loggedIn, err := s.tryPubkeyLoginHelper(username, getSecretKeyFn); err != nil {
		return err
	} else if loggedIn {
		return nil
	}

	return s.passphraseLogin(username, passphrase, nil, "")
}

func (s *LoginState) logout() error {
	s.G().Log.Debug("+ Logout called")
	s.Account(func(a *Account) {
		a.Logout()
	}, "LoginState - logout")
	s.G().Log.Debug("- Logout called")
	return nil
}

func (s *LoginState) Account(h acctHandler, name string) {
	s.G().Log.Debug("+ Account %q, putting in request chan", name)
	s.acctHandle(h, name)
	s.G().Log.Debug("- Account %q, done", name)
}

func (s *LoginState) PassphraseStreamCache(h func(*PassphraseStreamCache), name string) {
	s.Account(func(a *Account) {
		h(a.PassphraseStreamCache())
	}, name)
}

func (s *LoginState) LocalSession(h func(*Session), name string) {
	s.Account(func(a *Account) {
		h(a.LocalSession())
	}, name)
}

func (s *LoginState) LoginSession(h func(*LoginSession), name string) {
	s.Account(func(a *Account) {
		h(a.LoginSession())
	}, name)
}

func (s *LoginState) SecretSyncer(h func(*SecretSyncer), name string) {
	s.Account(func(a *Account) {
		// SecretSyncer needs session loaded:
		a.localSession.Load()
		h(a.SecretSyncer())
	}, name)
}

func (s *LoginState) RunSecretSyncer(uid *UID) error {
	var err error
	s.Account(func(a *Account) {
		err = a.RunSecretSyncer(uid)
	}, "RunSecretSyncer")
	return err
}

func (s *LoginState) Keyring(h func(*SKBKeyringFile), name string) error {
	var err error
	s.Account(func(a *Account) {
		var kr *SKBKeyringFile
		kr, err = a.Keyring()
		if err != nil {
			return
		}
		h(kr)
	}, name)
	return err
}

func (s *LoginState) LoggedIn() bool {
	var res bool
	s.Account(func(a *Account) {
		res = a.LoggedIn()
	}, "LoggedIn")
	return res
}

func (s *LoginState) LoggedInLoad() (lin bool, err error) {
	s.Account(func(a *Account) {
		lin, err = a.LoggedInLoad()
	}, "LoggedInLoad")
	return
}

func (s *LoginState) PassphraseStream() PassphraseStream {
	var pps PassphraseStream
	s.PassphraseStreamCache(func(s *PassphraseStreamCache) {
		pps = s.PassphraseStream()
	}, "PassphraseStream")
	return pps
}
