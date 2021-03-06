package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"text/template"

	v1 "github.com/decred/politeia/politeiawww/api/v1"
	"github.com/decred/politeia/politeiawww/user"
	"github.com/decred/politeia/util"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
)

var (
	templateNewUserEmail = template.Must(
		template.New("new_user_email_template").Parse(templateNewUserEmailRaw))
	templateResetPasswordEmail = template.Must(
		template.New("reset_password_email_template").Parse(templateResetPasswordEmailRaw))
	templateUpdateUserKeyEmail = template.Must(
		template.New("update_user_key_email_template").Parse(templateUpdateUserKeyEmailRaw))
	templateUserLockedResetPassword = template.Must(
		template.New("user_locked_reset_password").Parse(templateUserLockedResetPasswordRaw))
	templateUserPasswordChanged = template.Must(
		template.New("user_changed_password").Parse(templateUserPasswordChangedRaw))
)

// getSession returns the active cookie session.
func (p *politeiawww) getSession(r *http.Request) (*sessions.Session, error) {
	return p.store.Get(r, v1.CookieSession)
}

// isAdmin returns true if the current session has admin privileges.
func (p *politeiawww) isAdmin(w http.ResponseWriter, r *http.Request) (bool, error) {
	user, err := p.getSessionUser(w, r)
	if err != nil {
		return false, err
	}

	return user.Admin, nil
}

// getSessionUUID returns the uuid address of the currently logged in user from
// the session store.
func (p *politeiawww) getSessionUUID(r *http.Request) (string, error) {
	session, err := p.getSession(r)
	if err != nil {
		return "", err
	}

	id, ok := session.Values["uuid"].(string)
	if !ok {
		return "", ErrSessionUUIDNotFound
	}
	log.Tracef("getSessionUUID: %v", session.ID)

	return id, nil
}

// getSessionUser retrieves the current session user from the database.
func (p *politeiawww) getSessionUser(w http.ResponseWriter, r *http.Request) (*user.User, error) {
	id, err := p.getSessionUUID(r)
	if err != nil {
		return nil, err
	}

	log.Tracef("getSessionUser: %v", id)
	pid, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}

	user, err := p.db.UserGetById(pid)
	if err != nil {
		return nil, err
	}

	if user.Deactivated {
		p.removeSession(w, r)
		return nil, v1.UserError{
			ErrorCode: v1.ErrorStatusNotLoggedIn,
		}
	}

	return user, nil
}

// setSessionUserID sets the "uuid" session key to the provided value.
func (p *politeiawww) setSessionUserID(w http.ResponseWriter, r *http.Request, id string) error {
	log.Tracef("setSessionUserID: %v %v", id, v1.CookieSession)
	session, err := p.getSession(r)
	if err != nil {
		return err
	}

	session.Values["uuid"] = id
	return session.Save(r, w)
}

// removeSession deletes the session from the filesystem.
func (p *politeiawww) removeSession(w http.ResponseWriter, r *http.Request) error {
	log.Tracef("removeSession: %v", v1.CookieSession)
	session, err := p.getSession(r)
	if err != nil {
		return err
	}

	// Check for invalid session.
	if session.ID == "" {
		return nil
	}

	// Saving the session with a negative MaxAge will cause it to be deleted
	// from the filesystem.
	session.Options.MaxAge = -1
	return session.Save(r, w)
}

// handleNewUser handles the incoming new user command. It verifies that the new user
// doesn't already exist, and then creates a new user in the db and generates a random
// code used for verification. The code is intended to be sent to the specified email.
func (p *politeiawww) handleNewUser(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleNewUser")

	// Get the new user command.
	var u v1.NewUser
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&u); err != nil {
		RespondWithError(w, r, 0, "handleNewUser: unmarshal", v1.UserError{
			ErrorCode: v1.ErrorStatusInvalidInput,
		})
		return
	}

	reply, err := p.processNewUser(u)
	if err != nil {
		RespondWithError(w, r, 0, "handleNewUser: processNewUser %v", err)
		return
	}

	// Reply with the verification token.
	util.RespondWithJSON(w, http.StatusOK, reply)
}

// handleVerifyNewUser handles the incoming new user verify command. It verifies
// that the user with the provided email has a verification token that matches
// the provided token and that the verification token has not yet expired.
func (p *politeiawww) handleVerifyNewUser(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleVerifyNewUser")

	// Get the new user verify command.
	var vnu v1.VerifyNewUser
	err := util.ParseGetParams(r, &vnu)
	if err != nil {
		RespondWithError(w, r, 0, "handleVerifyNewUser: ParseGetParams",
			v1.UserError{
				ErrorCode: v1.ErrorStatusInvalidInput,
			})
		return
	}

	_, err = p.processVerifyNewUser(vnu)
	if err != nil {
		RespondWithError(w, r, 0, "handleVerifyNewUser: "+
			"processVerifyNewUser %v", err)
		return
	}

	util.RespondWithJSON(w, http.StatusOK, v1.VerifyNewUserReply{})
}

// handleResendVerification sends another verification email for new user
// signup, if there is an existing verification token and it is expired.
func (p *politeiawww) handleResendVerification(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleResendVerification")

	// Get the resend verification command.
	var rv v1.ResendVerification
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&rv); err != nil {
		RespondWithError(w, r, 0, "handleResendVerification: unmarshal",
			v1.UserError{
				ErrorCode: v1.ErrorStatusInvalidInput,
			})
		return
	}

	rvr, err := p.processResendVerification(&rv)
	if err != nil {
		RespondWithError(w, r, 0, "handleResendVerification: "+
			"processResendVerification %v", err)
		return
	}

	// Reply with the verification token.
	util.RespondWithJSON(w, http.StatusOK, *rvr)
}

// handleLogin handles the incoming login command.  It verifies that the user
// exists and the accompanying password.  On success a cookie is added to the
// gorilla sessions that must be returned on subsequent calls.
func (p *politeiawww) handleLogin(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleLogin")

	// Get the login command.
	var l v1.Login
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&l); err != nil {
		RespondWithError(w, r, 0, "handleLogin: failed to decode: %v", err)
		return
	}

	reply, err := p.processLogin(l)
	if err != nil {
		RespondWithError(w, r, http.StatusUnauthorized,
			"handleLogin: processLogin %v", err)
		return
	}

	// Mark user as logged in if there's no error.
	err = p.setSessionUserID(w, r, reply.UserID)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleLogin: setSessionUser %v", err)
		return
	}

	// Set session max age
	reply.SessionMaxAge = sessionMaxAge

	// Reply with the user information.
	util.RespondWithJSON(w, http.StatusOK, reply)
}

// handleLogout logs the user out.
func (p *politeiawww) handleLogout(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleLogout")

	_, err := p.getSessionUser(w, r)
	if err != nil {
		RespondWithError(w, r, 0, "handleLogout: getSessionUser", v1.UserError{
			ErrorCode: v1.ErrorStatusNotLoggedIn,
		})
		return
	}

	err = p.removeSession(w, r)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleLogout: removeSession %v", err)
		return
	}

	// Reply with the user information.
	var reply v1.LogoutReply
	util.RespondWithJSON(w, http.StatusOK, reply)
}

// handleResetPassword handles the reset password command.
func (p *politeiawww) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	log.Trace("handleResetPassword")

	// Get the reset password command.
	var rp v1.ResetPassword
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&rp); err != nil {
		RespondWithError(w, r, 0, "handleResetPassword: unmarshal",
			v1.UserError{
				ErrorCode: v1.ErrorStatusInvalidInput,
			})
		return
	}

	rpr, err := p.processResetPassword(rp)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleResetPassword: processResetPassword %v", err)
		return
	}

	// Reply with the error code.
	util.RespondWithJSON(w, http.StatusOK, rpr)
}

// handleUserDetails handles fetching user details by user id.
func (p *politeiawww) handleUserDetails(w http.ResponseWriter, r *http.Request) {
	// Add the path param to the struct.
	log.Tracef("handleUserDetails")
	pathParams := mux.Vars(r)
	var ud v1.UserDetails
	ud.UserID = pathParams["userid"]

	userID, err := uuid.Parse(ud.UserID)
	if err != nil {
		RespondWithError(w, r, 0, "handleUserDetails: ParseUint",
			v1.UserError{
				ErrorCode: v1.ErrorStatusInvalidInput,
			})
		return
	}

	user, err := p.getSessionUser(w, r)
	if err != nil {
		// This is a public route so a logged in user is not required
		log.Debugf("handleUserDetails: could not get session user: %v", err)
	}

	udr, err := p.processUserDetails(&ud,
		user != nil && user.ID == userID,
		user != nil && user.Admin,
	)

	if err != nil {
		RespondWithError(w, r, 0,
			"handleUserDetails: processUserDetails %v", err)
		return
	}

	// Reply with the proposal details.
	util.RespondWithJSON(w, http.StatusOK, udr)
}

// handleSecret is a mock handler to test privileged routes.
func (p *politeiawww) handleSecret(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleSecret")

	fmt.Fprintf(w, "secret sauce")
}

// handleMe returns logged in user information.
func (p *politeiawww) handleMe(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleMe")

	user, err := p.getSessionUser(w, r)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleMe: getSessionUser %v", err)
		return
	}

	reply, err := p.createLoginReply(user, user.LastLoginTime)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleMe: createLoginReply %v", err)
		return
	}

	// Set session max age
	reply.SessionMaxAge = sessionMaxAge

	util.RespondWithJSON(w, http.StatusOK, *reply)
}

// handleUpdateUserKey handles the incoming update user key command. It generates
// a random code used for verification. The code is intended to be sent to the
// email of the logged in user.
func (p *politeiawww) handleUpdateUserKey(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleUpdateUserKey")

	// Get the update user key command.
	var u v1.UpdateUserKey
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&u); err != nil {
		RespondWithError(w, r, 0, "handleUpdateUserKey: unmarshal", v1.UserError{
			ErrorCode: v1.ErrorStatusInvalidInput,
		})
		return
	}

	user, err := p.getSessionUser(w, r)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleUpdateUserKey: getSessionUser %v", err)
		return
	}

	reply, err := p.processUpdateUserKey(user, u)
	if err != nil {
		RespondWithError(w, r, 0, "handleUpdateUserKey: processUpdateUserKey %v", err)
		return
	}

	// Reply with the verification token.
	util.RespondWithJSON(w, http.StatusOK, reply)
}

// handleVerifyUpdateUserKey handles the incoming update user key verify command. It verifies
// that the user with the provided email has a verification token that matches
// the provided token and that the verification token has not yet expired.
func (p *politeiawww) handleVerifyUpdateUserKey(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleVerifyUpdateUserKey")

	// Get the new user verify command.
	var vuu v1.VerifyUpdateUserKey
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&vuu); err != nil {
		RespondWithError(w, r, 0, "handleVerifyUpdateUserKey: unmarshal",
			v1.UserError{
				ErrorCode: v1.ErrorStatusInvalidInput,
			})
		return
	}

	user, err := p.getSessionUser(w, r)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleVerifyUpdateUserKey: getSessionUser %v", err)
		return
	}

	_, err = p.processVerifyUpdateUserKey(user, vuu)
	if err != nil {
		RespondWithError(w, r, 0, "handleVerifyUpdateUserKey: "+
			"processVerifyUpdateUserKey %v", err)
		return
	}

	util.RespondWithJSON(w, http.StatusOK, v1.VerifyUpdateUserKeyReply{})
}

// handleChangeUsername handles the change user name command.
func (p *politeiawww) handleChangeUsername(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleChangeUsername")

	// Get the change username command.
	var cu v1.ChangeUsername
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&cu); err != nil {
		RespondWithError(w, r, 0, "handleChangeUsername: unmarshal",
			v1.UserError{
				ErrorCode: v1.ErrorStatusInvalidInput,
			})
		return
	}

	user, err := p.getSessionUser(w, r)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleChangeUsername: getSessionUser %v", err)
		return
	}

	reply, err := p.processChangeUsername(user.Email, cu)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleChangeUsername: processChangeUsername %v", err)
		return
	}

	// Reply with the error code.
	util.RespondWithJSON(w, http.StatusOK, reply)
}

// handleChangePassword handles the change password command.
func (p *politeiawww) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleChangePassword")

	// Get the change password command.
	var cp v1.ChangePassword
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&cp); err != nil {
		RespondWithError(w, r, 0, "handleChangePassword: unmarshal",
			v1.UserError{
				ErrorCode: v1.ErrorStatusInvalidInput,
			})
		return
	}

	user, err := p.getSessionUser(w, r)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleChangePassword: getSessionUser %v", err)
		return
	}

	reply, err := p.processChangePassword(user.Email, cp)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleChangePassword: processChangePassword %v", err)
		return
	}

	// Reply with the error code.
	util.RespondWithJSON(w, http.StatusOK, reply)
}

// handleVerifyUserPayment checks whether the provided transaction
// is on the blockchain and meets the requirements to consider the user
// registration fee as paid.
func (p *politeiawww) handleVerifyUserPayment(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleVerifyUserPayment")

	// Get the verify user payment tx command.
	var vupt v1.VerifyUserPayment
	err := util.ParseGetParams(r, &vupt)
	if err != nil {
		RespondWithError(w, r, 0, "handleVerifyUserPayment: ParseGetParams",
			v1.UserError{
				ErrorCode: v1.ErrorStatusInvalidInput,
			})
		return
	}

	user, err := p.getSessionUser(w, r)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleVerifyUserPayment: getSessionUser %v", err)
		return
	}

	vuptr, err := p.processVerifyUserPayment(user, vupt)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleVerifyUserPayment: processVerifyUserPayment %v",
			err)
		return
	}

	util.RespondWithJSON(w, http.StatusOK, vuptr)
}

// handleEditUser handles editing a user's preferences.
func (p *politeiawww) handleEditUser(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleEditUser")

	var eu v1.EditUser
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&eu); err != nil {
		RespondWithError(w, r, 0, "handleEditUser: unmarshal",
			v1.UserError{
				ErrorCode: v1.ErrorStatusInvalidInput,
			})
		return
	}

	adminUser, err := p.getSessionUser(w, r)
	if err != nil {
		RespondWithError(w, r, 0, "handleEditUser: getSessionUser %v",
			err)
		return
	}

	eur, err := p.processEditUser(&eu, adminUser)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleEditUser: processEditUser %v", err)
		return
	}

	util.RespondWithJSON(w, http.StatusOK, eur)
}

// handleUsers handles fetching a list of users.
func (p *politeiawww) handleUsers(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleUsers")

	var u v1.Users
	err := util.ParseGetParams(r, &u)
	if err != nil {
		RespondWithError(w, r, 0, "handleUsers: ParseGetParams",
			v1.UserError{
				ErrorCode: v1.ErrorStatusInvalidInput,
			})
		return
	}

	ur, err := p.processUsers(&u)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleUsers: processUsers %v", err)
		return
	}

	util.RespondWithJSON(w, http.StatusOK, ur)
}

// handleUserPaymentsRescan allows an admin to rescan a user's paywall address
// to check for any payments that may have been missed by paywall polling.
func (p *politeiawww) handleUserPaymentsRescan(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleUserPaymentsRescan")

	var upr v1.UserPaymentsRescan
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&upr); err != nil {
		RespondWithError(w, r, 0, "handleUserPaymentsRescan: unmarshal",
			v1.UserError{
				ErrorCode: v1.ErrorStatusInvalidInput,
			})
		return
	}

	reply, err := p.processUserPaymentsRescan(upr)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleUserPaymentsRescan: processUserPaymentsRescan:  %v",
			err)
		return
	}

	util.RespondWithJSON(w, http.StatusOK, reply)
}

// handleManageUser handles editing a user's details.
func (p *politeiawww) handleManageUser(w http.ResponseWriter, r *http.Request) {
	log.Tracef("handleManageUser")

	var mu v1.ManageUser
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&mu); err != nil {
		RespondWithError(w, r, 0, "handleManageUser: unmarshal",
			v1.UserError{
				ErrorCode: v1.ErrorStatusInvalidInput,
			})
		return
	}

	adminUser, err := p.getSessionUser(w, r)
	if err != nil {
		RespondWithError(w, r, 0, "handleManageUser: getSessionUser %v",
			err)
		return
	}

	mur, err := p.processManageUser(&mu, adminUser)
	if err != nil {
		RespondWithError(w, r, 0,
			"handleManageUser: processManageUser %v", err)
		return
	}

	util.RespondWithJSON(w, http.StatusOK, mur)
}

// setUserWWWRoutes setsup the user routes.
func (p *politeiawww) setUserWWWRoutes() {
	// Public routes
	p.addRoute(http.MethodPost, v1.RouteNewUser, p.handleNewUser,
		permissionPublic)
	p.addRoute(http.MethodGet, v1.RouteVerifyNewUser,
		p.handleVerifyNewUser, permissionPublic)
	p.addRoute(http.MethodPost, v1.RouteResendVerification,
		p.handleResendVerification, permissionPublic)
	p.addRoute(http.MethodPost, v1.RouteLogin, p.handleLogin,
		permissionPublic)
	p.addRoute(http.MethodPost, v1.RouteLogout, p.handleLogout,
		permissionPublic)
	p.addRoute(http.MethodPost, v1.RouteResetPassword,
		p.handleResetPassword, permissionPublic)
	p.addRoute(http.MethodGet, v1.RouteUserDetails,
		p.handleUserDetails, permissionPublic)

	// Routes that require being logged in.
	p.addRoute(http.MethodPost, v1.RouteSecret, p.handleSecret,
		permissionLogin)
	p.addRoute(http.MethodGet, v1.RouteUserMe, p.handleMe, permissionLogin)
	p.addRoute(http.MethodPost, v1.RouteUpdateUserKey,
		p.handleUpdateUserKey, permissionLogin)
	p.addRoute(http.MethodPost, v1.RouteVerifyUpdateUserKey,
		p.handleVerifyUpdateUserKey, permissionLogin)
	p.addRoute(http.MethodPost, v1.RouteChangeUsername,
		p.handleChangeUsername, permissionLogin)
	p.addRoute(http.MethodPost, v1.RouteChangePassword,
		p.handleChangePassword, permissionLogin)
	p.addRoute(http.MethodGet, v1.RouteVerifyUserPayment,
		p.handleVerifyUserPayment, permissionLogin)
	p.addRoute(http.MethodPost, v1.RouteEditUser,
		p.handleEditUser, permissionLogin)

	// Routes that require being logged in as an admin user.
	p.addRoute(http.MethodGet, v1.RouteUsers,
		p.handleUsers, permissionAdmin)
	p.addRoute(http.MethodPut, v1.RouteUserPaymentsRescan,
		p.handleUserPaymentsRescan, permissionAdmin)
	p.addRoute(http.MethodPost, v1.RouteManageUser,
		p.handleManageUser, permissionAdmin)
}
