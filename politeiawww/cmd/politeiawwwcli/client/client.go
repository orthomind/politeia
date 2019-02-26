// Copyright (c) 2018-2019 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"reflect"
	"strings"

	"github.com/decred/dcrwallet/rpc/walletrpc"
	"github.com/decred/politeia/politeiawww/api/v1"
	"github.com/decred/politeia/util"
	"github.com/gorilla/schema"
	"golang.org/x/net/publicsuffix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/decred/politeia/politeiawww/cmd/politeiawwwcli/config"
)

// Client is a politeiawww client.
type Client struct {
	http *http.Client
	cfg  *config.Config

	// wallet grpc
	ctx    context.Context
	creds  credentials.TransportCredentials
	conn   *grpc.ClientConn
	wallet walletrpc.WalletServiceClient
}

func prettyPrintJSON(v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("MarshalIndent: %v", err)
	}
	fmt.Fprintf(os.Stdout, "%s\n", b)
	return nil
}

func (c *Client) makeRequest(method, route string, body interface{}) ([]byte, error) {
	// Setup request
	var requestBody []byte
	var queryParams string
	if body != nil {
		switch {
		case method == http.MethodGet:

			// Use reflection in case the interface value is nil
			// but the interface type is not. This can happen when
			// query params exist but are not used.
			if reflect.ValueOf(body).IsNil() {
				break
			}

			// GET requests don't have a request body; instead we
			// will populate the query params.
			form := url.Values{}
			if err := schema.NewEncoder().Encode(body, form); err != nil {
				return nil, err
			}
			queryParams = "?" + form.Encode()
		case method == http.MethodPost || method == http.MethodPut:
			var err error
			requestBody, err = json.Marshal(body)
			if err != nil {
				return nil, err
			}
		}
	}

	fullRoute := c.cfg.Host + v1.PoliteiaWWWAPIRoute + route + queryParams

	// Print request details
	switch {
	case c.cfg.Verbose && method == http.MethodGet:
		fmt.Printf("Request: GET %v\n", fullRoute)
	case c.cfg.Verbose && method == http.MethodPost:
		fmt.Printf("Request: POST %v\n", fullRoute)
		err := prettyPrintJSON(body)
		if err != nil {
			return nil, err
		}
	case c.cfg.Verbose && method == http.MethodPut:
		fmt.Printf("Request: PUT %v\n", fullRoute)
		err := prettyPrintJSON(body)
		if err != nil {
			return nil, err
		}
	}

	// Create http request
	req, err := http.NewRequest(method, fullRoute, bytes.NewReader(requestBody))
	if err != nil {
		return nil, err
	}
	req.Header.Add(v1.CsrfToken, c.cfg.CSRF)

	// Send request
	r, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		r.Body.Close()
	}()

	responseBody := util.ConvertBodyToByteArray(r.Body, false)

	// Validate response status
	if r.StatusCode != http.StatusOK {
		var ue v1.UserError
		err = json.Unmarshal(responseBody, &ue)
		if err == nil && ue.ErrorCode != 0 {
			return nil, fmt.Errorf("%v, %v %v", r.StatusCode,
				v1.ErrorStatus[ue.ErrorCode], strings.Join(ue.ErrorContext, ", "))
		}

		return nil, fmt.Errorf("%v", r.StatusCode)
	}

	// Print response details
	if c.cfg.Verbose {
		fmt.Printf("Response: %v\n", r.StatusCode)
	}

	return responseBody, nil
}

// Version returns the version information for the politeiawww instance.
func (c *Client) Version() (*v1.VersionReply, error) {
	fullRoute := c.cfg.Host + v1.PoliteiaWWWAPIRoute + v1.RouteVersion

	// Print request details
	if c.cfg.Verbose {
		fmt.Printf("Request: GET %v\n", fullRoute)
	}

	// Create new http request instead of using makeRequest()
	// so that we can save the CSRF tokens to disk.
	req, err := http.NewRequest("GET", fullRoute, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add(v1.CsrfToken, c.cfg.CSRF)

	// Send request
	r, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		r.Body.Close()
	}()

	responseBody := util.ConvertBodyToByteArray(r.Body, false)

	// Validate response status
	if r.StatusCode != http.StatusOK {
		var ue v1.UserError
		err = json.Unmarshal(responseBody, &ue)
		if err == nil {
			return nil, fmt.Errorf("%v, %v %v", r.StatusCode,
				v1.ErrorStatus[ue.ErrorCode], strings.Join(ue.ErrorContext, ", "))
		}

		return nil, fmt.Errorf("%v", r.StatusCode)
	}

	// Unmarshal response
	var vr v1.VersionReply
	err = json.Unmarshal(responseBody, &vr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal VersionReply: %v", err)
	}

	// Print response details
	if c.cfg.Verbose {
		fmt.Printf("Response: %v\n", r.StatusCode)
		err := prettyPrintJSON(vr)
		if err != nil {
			return nil, err
		}
	}

	// CSRF protection works via the double-submit method.
	// One token is sent in the cookie. A second token is
	// sent in the header. Both tokens must be persisted
	// between CLI commands.

	// Persist CSRF header token
	c.cfg.CSRF = r.Header.Get(v1.CsrfToken)
	err = c.cfg.SaveCSRF(c.cfg.CSRF)
	if err != nil {
		return nil, err
	}

	// Persist CSRF cookie token
	err = c.cfg.SaveCookies(c.http.Jar.Cookies(req.URL))
	if err != nil {
		return nil, err
	}

	return &vr, nil
}

// Login logs a user into politeiawww.
func (c *Client) Login(l *v1.Login) (*v1.LoginReply, error) {
	// Setup request
	requestBody, err := json.Marshal(l)
	if err != nil {
		return nil, err
	}

	fullRoute := c.cfg.Host + v1.PoliteiaWWWAPIRoute + v1.RouteLogin

	// Print request details
	if c.cfg.Verbose {
		fmt.Printf("Request: POST %v\n", fullRoute)
		err := prettyPrintJSON(l)
		if err != nil {
			return nil, err
		}
	}

	// Create new http request instead of using makeRequest()
	// so that we can save the session data for subsequent
	// commands
	req, err := http.NewRequest("POST", fullRoute, bytes.NewReader(requestBody))
	if err != nil {
		return nil, err
	}
	req.Header.Add(v1.CsrfToken, c.cfg.CSRF)

	// Send request
	r, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		r.Body.Close()
	}()

	responseBody := util.ConvertBodyToByteArray(r.Body, false)

	// Validate response status
	if r.StatusCode != http.StatusOK {
		var ue v1.UserError
		err = json.Unmarshal(responseBody, &ue)
		if err == nil {
			return nil, fmt.Errorf("%v, %v %v", r.StatusCode,
				v1.ErrorStatus[ue.ErrorCode], strings.Join(ue.ErrorContext, ", "))
		}

		return nil, fmt.Errorf("%v", r.StatusCode)
	}

	// Unmarshal response
	var lr v1.LoginReply
	err = json.Unmarshal(responseBody, &lr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal LoginReply: %v", err)
	}

	// Print response details
	if c.cfg.Verbose {
		fmt.Printf("Response: %v\n", r.StatusCode)
		err := prettyPrintJSON(lr)
		if err != nil {
			return nil, err
		}
	}

	// Persist session data
	ck := c.http.Jar.Cookies(req.URL)
	if err = c.cfg.SaveCookies(ck); err != nil {
		return nil, err
	}

	return &lr, nil
}

// Logout logs out a user from politeiawww.
func (c *Client) Logout() (*v1.LogoutReply, error) {
	fullRoute := c.cfg.Host + v1.PoliteiaWWWAPIRoute + v1.RouteLogout

	// Print request details
	if c.cfg.Verbose {
		fmt.Printf("Request: POST  %v\n", fullRoute)
	}

	// Create new http request instead of using makeRequest()
	// so that we can save the updated cookies to disk
	req, err := http.NewRequest("POST", fullRoute, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Add(v1.CsrfToken, c.cfg.CSRF)

	// Send request
	r, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		r.Body.Close()
	}()

	responseBody := util.ConvertBodyToByteArray(r.Body, false)

	// Validate response status
	if r.StatusCode != http.StatusOK {
		var ue v1.UserError
		err = json.Unmarshal(responseBody, &ue)
		if err == nil {
			return nil, fmt.Errorf("%v, %v %v", r.StatusCode,
				v1.ErrorStatus[ue.ErrorCode], strings.Join(ue.ErrorContext, ", "))
		}

		return nil, fmt.Errorf("%v", r.StatusCode)
	}

	// Unmarshal response
	var lr v1.LogoutReply
	err = json.Unmarshal(responseBody, &lr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal LogoutReply: %v", err)
	}

	// Print response details
	if c.cfg.Verbose {
		fmt.Printf("Response: %v\n", r.StatusCode)
		err := prettyPrintJSON(lr)
		if err != nil {
			return nil, err
		}
	}

	// Persist cookies
	ck := c.http.Jar.Cookies(req.URL)
	if err = c.cfg.SaveCookies(ck); err != nil {
		return nil, err
	}

	return &lr, nil
}

// Policy returns the politeiawww policy information.
func (c *Client) Policy() (*v1.PolicyReply, error) {
	responseBody, err := c.makeRequest("GET", v1.RoutePolicy, nil)
	if err != nil {
		return nil, err
	}

	var pr v1.PolicyReply
	err = json.Unmarshal(responseBody, &pr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal PolicyReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(pr)
		if err != nil {
			return nil, err
		}
	}

	return &pr, nil
}

// NewUser creates a new politeiawww user.
func (c *Client) NewUser(nu *v1.NewUser) (*v1.NewUserReply, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteNewUser, nu)
	if err != nil {
		return nil, err
	}

	var nur v1.NewUserReply
	err = json.Unmarshal(responseBody, &nur)
	if err != nil {
		return nil, fmt.Errorf("unmarshal NewUserReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(nur)
		if err != nil {
			return nil, err
		}
	}

	return &nur, nil
}

// VerifyNewUser verifies a user's email address.
func (c *Client) VerifyNewUser(vnu *v1.VerifyNewUser) (*v1.VerifyNewUserReply, error) {
	responseBody, err := c.makeRequest("GET", "/user/verify", vnu)
	if err != nil {
		return nil, err
	}

	var vnur v1.VerifyNewUserReply
	err = json.Unmarshal(responseBody, &vnur)
	if err != nil {
		return nil, fmt.Errorf("unmarshal VerifyNewUserReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(vnur)
		if err != nil {
			return nil, err
		}
	}

	return &vnur, nil
}

// Me returns user details for the logged in user.
func (c *Client) Me() (*v1.LoginReply, error) {
	responseBody, err := c.makeRequest("GET", v1.RouteUserMe, nil)
	if err != nil {
		return nil, err
	}

	var lr v1.LoginReply
	err = json.Unmarshal(responseBody, &lr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal LoginReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(lr)
		if err != nil {
			return nil, err
		}
	}

	return &lr, nil
}

// Secret pings politeiawww.
func (c *Client) Secret() (*v1.UserError, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteSecret, v1.Login{})
	if err != nil {
		return nil, err
	}

	var ue v1.UserError
	err = json.Unmarshal(responseBody, &ue)
	if err != nil {
		return nil, fmt.Errorf("unmarshal UserError: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(ue)
		if err != nil {
			return nil, err
		}
	}

	return &ue, nil
}

// ChangeUsername changes the username of the logged in user.
func (c *Client) ChangeUsername(cu *v1.ChangeUsername) (*v1.ChangeUsernameReply, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteChangeUsername, cu)
	if err != nil {
		return nil, err
	}

	var cur v1.ChangeUsernameReply
	err = json.Unmarshal(responseBody, &cur)
	if err != nil {
		return nil, fmt.Errorf("unmarshal ChangeUsernameReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(cur)
		if err != nil {
			return nil, err
		}
	}

	return &cur, nil
}

// ChangePassword changes the password for the logged in user.
func (c *Client) ChangePassword(cp *v1.ChangePassword) (*v1.ChangePasswordReply, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteChangePassword, cp)
	if err != nil {
		return nil, err
	}

	var cpr v1.ChangePasswordReply
	err = json.Unmarshal(responseBody, &cpr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal ChangePasswordReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(cpr)
		if err != nil {
			return nil, err
		}
	}

	return &cpr, nil
}

// ResetPassword resets the password of the specified user.
func (c *Client) ResetPassword(rp *v1.ResetPassword) (*v1.ResetPasswordReply, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteResetPassword, rp)
	if err != nil {
		return nil, err
	}

	var rpr v1.ResetPasswordReply
	err = json.Unmarshal(responseBody, &rpr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal ResetPasswordReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(rpr)
		if err != nil {
			return nil, err
		}
	}

	return &rpr, nil
}

// ProposalPaywallDetails retrieves proposal credit paywall information for the
// logged in user.
func (c *Client) ProposalPaywallDetails() (*v1.ProposalPaywallDetailsReply, error) {
	responseBody, err := c.makeRequest("GET",
		v1.RouteProposalPaywallDetails, nil)
	if err != nil {
		return nil, err
	}

	var ppdr v1.ProposalPaywallDetailsReply
	err = json.Unmarshal(responseBody, &ppdr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal ProposalPaywalDetailsReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(ppdr)
		if err != nil {
			return nil, err
		}
	}

	return &ppdr, nil
}

// NewProposal submits the specified proposal to politeiawww for the logged in
// user.
func (c *Client) NewProposal(np *v1.NewProposal) (*v1.NewProposalReply, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteNewProposal, np)
	if err != nil {
		return nil, err
	}

	var npr v1.NewProposalReply
	err = json.Unmarshal(responseBody, &npr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal NewProposalReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(npr)
		if err != nil {
			return nil, err
		}
	}

	return &npr, nil
}

// EditProposal edits the specified proposal with the logged in user.
func (c *Client) EditProposal(ep *v1.EditProposal) (*v1.EditProposalReply, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteEditProposal, ep)
	if err != nil {
		return nil, err
	}

	var epr v1.EditProposalReply
	err = json.Unmarshal(responseBody, &epr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal EditProposalReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(epr)
		if err != nil {
			return nil, err
		}
	}

	return &epr, nil
}

// ProposalDetails retrieves the specified proposal.
func (c *Client) ProposalDetails(token string, pd *v1.ProposalsDetails) (*v1.ProposalDetailsReply, error) {
	responseBody, err := c.makeRequest("GET", "/proposals/"+token, pd)
	if err != nil {
		return nil, err
	}

	var pr v1.ProposalDetailsReply
	err = json.Unmarshal(responseBody, &pr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal ProposalDetailsReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(pr)
		if err != nil {
			return nil, err
		}
	}

	return &pr, nil
}

// UserProposals retrieves the proposals that have been submitted by the
// specified user.
func (c *Client) UserProposals(up *v1.UserProposals) (*v1.UserProposalsReply, error) {
	responseBody, err := c.makeRequest("GET", v1.RouteUserProposals, up)
	if err != nil {
		return nil, err
	}

	var upr v1.UserProposalsReply
	err = json.Unmarshal(responseBody, &upr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal UserProposalsReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(upr)
		if err != nil {
			return nil, err
		}
	}

	return &upr, nil
}

// SetProposalStatus changes the status of the specified proposal.
func (c *Client) SetProposalStatus(sps *v1.SetProposalStatus) (*v1.SetProposalStatusReply, error) {
	route := "/proposals/" + sps.Token + "/status"
	responseBody, err := c.makeRequest("POST", route, sps)
	if err != nil {
		return nil, err
	}

	var spsr v1.SetProposalStatusReply
	err = json.Unmarshal(responseBody, &spsr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal SetProposalStatusReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(spsr)
		if err != nil {
			return nil, err
		}
	}

	return &spsr, nil
}

// GetAllVetted retrieves a page of vetted proposals.
func (c *Client) GetAllVetted(gav *v1.GetAllVetted) (*v1.GetAllVettedReply, error) {
	responseBody, err := c.makeRequest("GET", v1.RouteAllVetted, gav)
	if err != nil {
		return nil, err
	}

	var gavr v1.GetAllVettedReply
	err = json.Unmarshal(responseBody, &gavr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal GetAllVettedReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(gavr)
		if err != nil {
			return nil, err
		}
	}

	return &gavr, nil
}

// GetAllUnvetted retrieves a page of unvetted proposals.
func (c *Client) GetAllUnvetted(gau *v1.GetAllUnvetted) (*v1.GetAllUnvettedReply, error) {
	responseBody, err := c.makeRequest("GET", v1.RouteAllUnvetted, gau)
	if err != nil {
		return nil, err
	}

	var gaur v1.GetAllUnvettedReply
	err = json.Unmarshal(responseBody, &gaur)
	if err != nil {
		return nil, fmt.Errorf("unmarshal GetAllUnvettedReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(gaur)
		if err != nil {
			return nil, err
		}
	}

	return &gaur, nil
}

// NewComment submits a new proposal comment for the logged in user.
func (c *Client) NewComment(nc *v1.NewComment) (*v1.NewCommentReply, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteNewComment, nc)
	if err != nil {
		return nil, err
	}

	var ncr v1.NewCommentReply
	err = json.Unmarshal(responseBody, &ncr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal NewCommentReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(ncr)
		if err != nil {
			return nil, err
		}
	}

	return &ncr, nil
}

// GetComments retrieves the comments for the specified proposal.
func (c *Client) GetComments(token string) (*v1.GetCommentsReply, error) {
	responseBody, err := c.makeRequest("GET", "/proposals/"+token+"/comments",
		nil)
	if err != nil {
		return nil, err
	}

	var gcr v1.GetCommentsReply
	err = json.Unmarshal(responseBody, &gcr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal GetCommentsReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(gcr)
		if err != nil {
			return nil, err
		}
	}

	return &gcr, nil
}

// UserCommentsLikes retrieves the comment likes (upvotes/downvotes) for the
// specified proposal that are from the logged in user.
func (c *Client) UserCommentsLikes(token string) (*v1.UserCommentsLikesReply, error) {
	route := "/user/proposals/" + token + "/commentslikes"
	responseBody, err := c.makeRequest("GET", route, nil)
	if err != nil {
		return nil, err
	}

	var uclr v1.UserCommentsLikesReply
	err = json.Unmarshal(responseBody, &uclr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal UserCommentsLikesReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(uclr)
		if err != nil {
			return nil, err
		}
	}

	return &uclr, nil
}

// LikeComment casts a like comment action (upvote/downvote) for the logged in
// user.
func (c *Client) LikeComment(lc *v1.LikeComment) (*v1.LikeCommentReply, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteLikeComment, lc)
	if err != nil {
		return nil, err
	}

	var lcr v1.LikeCommentReply
	err = json.Unmarshal(responseBody, &lcr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal LikeCommentReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(lcr)
		if err != nil {
			return nil, err
		}
	}

	return &lcr, nil
}

// CensorComment censors the specified proposal comment.
func (c *Client) CensorComment(cc *v1.CensorComment) (*v1.CensorCommentReply, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteCensorComment, cc)
	if err != nil {
		return nil, err
	}

	var ccr v1.CensorCommentReply
	err = json.Unmarshal(responseBody, &ccr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal CensorCommentReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(ccr)
		if err != nil {
			return nil, err
		}
	}

	return &ccr, nil
}

// StartVote starts the voting period for the specified proposal.
func (c *Client) StartVote(sv *v1.StartVote) (*v1.StartVoteReply, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteStartVote, sv)
	if err != nil {
		return nil, err
	}

	var svr v1.StartVoteReply
	err = json.Unmarshal(responseBody, &svr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal StartVoteReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(svr)
		if err != nil {
			return nil, err
		}
	}

	return &svr, nil
}

// VerifyUserPayment checks whether the logged in user has paid their user
// registration fee.
func (c *Client) VerifyUserPayment() (*v1.VerifyUserPaymentReply, error) {
	responseBody, err := c.makeRequest("GET", v1.RouteVerifyUserPayment, nil)
	if err != nil {
		return nil, err
	}

	var vupr v1.VerifyUserPaymentReply
	err = json.Unmarshal(responseBody, &vupr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal VerifyUserPaymentReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(vupr)
		if err != nil {
			return nil, err
		}
	}

	return &vupr, nil
}

// VoteResults retrieves the vote results for the specified proposal.
func (c *Client) VoteResults(token string) (*v1.VoteResultsReply, error) {
	responseBody, err := c.makeRequest("GET", "/proposals/"+token+"/votes", nil)
	if err != nil {
		return nil, err
	}

	var vrr v1.VoteResultsReply
	err = json.Unmarshal(responseBody, &vrr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal ProposalVotesReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(vrr)
		if err != nil {
			return nil, err
		}
	}

	return &vrr, nil
}

// UserDetails retrieves the user details for the specified user.
func (c *Client) UserDetails(userID string) (*v1.UserDetailsReply, error) {
	responseBody, err := c.makeRequest("GET", "/user/"+userID, nil)
	if err != nil {
		return nil, err
	}

	var udr v1.UserDetailsReply
	err = json.Unmarshal(responseBody, &udr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal UserDetailsReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(udr)
		if err != nil {
			return nil, err
		}
	}

	return &udr, nil
}

// Users retrieves a list of users that adhere to the specified filtering
// parameters.
func (c *Client) Users(u *v1.Users) (*v1.UsersReply, error) {
	responseBody, err := c.makeRequest("GET", v1.RouteUsers, u)
	if err != nil {
		return nil, err
	}

	var ur v1.UsersReply
	err = json.Unmarshal(responseBody, &ur)
	if err != nil {
		return nil, fmt.Errorf("unmarshal UsersReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(ur)
		if err != nil {
			return nil, err
		}
	}

	return &ur, nil
}

// ManageUser allows an admin to edit certain attributes of the specified user.
func (c *Client) ManageUser(mu *v1.ManageUser) (*v1.ManageUserReply, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteManageUser, mu)
	if err != nil {
		return nil, err
	}

	var mur v1.ManageUserReply
	err = json.Unmarshal(responseBody, &mur)
	if err != nil {
		return nil, fmt.Errorf("unmarshal ManageUserReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(mur)
		if err != nil {
			return nil, err
		}
	}

	return &mur, nil
}

// EditUser allows the logged in user to update their user settings.
func (c *Client) EditUser(eu *v1.EditUser) (*v1.EditUserReply, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteEditUser, eu)
	if err != nil {
		return nil, err
	}

	var eur v1.EditUserReply
	err = json.Unmarshal(responseBody, &eur)
	if err != nil {
		return nil, fmt.Errorf("unmarshal EditUserReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(eur)
		if err != nil {
			return nil, err
		}
	}

	return &eur, nil
}

// AuthorizeVote authorizes the voting period for the specified proposal using
// the logged in user.
func (c *Client) AuthorizeVote(av *v1.AuthorizeVote) (*v1.AuthorizeVoteReply, error) {
	responseBody, err := c.makeRequest("POST", "/proposals/authorizevote", av)
	if err != nil {
		return nil, err
	}

	var avr v1.AuthorizeVoteReply
	err = json.Unmarshal(responseBody, &avr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal AuthorizeVoteReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(avr)
		if err != nil {
			return nil, err
		}
	}

	return &avr, nil
}

// VoteStatus retrieves the vote status for the specified proposal.
func (c *Client) VoteStatus(token string) (*v1.VoteStatusReply, error) {
	route := "/proposals/" + token + "/votestatus"
	responseBody, err := c.makeRequest("GET", route, nil)
	if err != nil {
		return nil, err
	}

	var vsr v1.VoteStatusReply
	err = json.Unmarshal(responseBody, &vsr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal VoteStatusReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(vsr)
		if err != nil {
			return nil, err
		}
	}

	return &vsr, nil
}

// GetAllVoteStatus retreives the vote status of all public proposals.
func (c *Client) GetAllVoteStatus() (*v1.GetAllVoteStatusReply, error) {
	responseBody, err := c.makeRequest("GET", v1.RouteAllVoteStatus, nil)
	if err != nil {
		return nil, err
	}

	var avsr v1.GetAllVoteStatusReply
	err = json.Unmarshal(responseBody, &avsr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal GetAllVoteStatusReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(avsr)
		if err != nil {
			return nil, fmt.Errorf("prettyPrintJSON: %v", err)
		}
	}

	return &avsr, nil
}

// ActiveVotes retreives all proposals that are currently being voted on.
func (c *Client) ActiveVotes() (*v1.ActiveVoteReply, error) {
	responseBody, err := c.makeRequest("GET", v1.RouteActiveVote, nil)
	if err != nil {
		return nil, err
	}

	var avr v1.ActiveVoteReply
	err = json.Unmarshal(responseBody, &avr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal ActiveVoteReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(avr)
		if err != nil {
			return nil, err
		}
	}

	return &avr, nil
}

// CastVotes casts votes for a proposal.
func (c *Client) CastVotes(b *v1.Ballot) (*v1.BallotReply, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteCastVotes, &b)
	if err != nil {
		return nil, err
	}

	var br v1.BallotReply
	err = json.Unmarshal(responseBody, &br)
	if err != nil {
		return nil, fmt.Errorf("unmarshal BallotReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(br)
		if err != nil {
			return nil, err
		}
	}

	return &br, nil
}

// UpdateUserKey updates the identity of the logged in user.
func (c *Client) UpdateUserKey(uuk *v1.UpdateUserKey) (*v1.UpdateUserKeyReply, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteUpdateUserKey, &uuk)
	if err != nil {
		return nil, err
	}

	var uukr v1.UpdateUserKeyReply
	err = json.Unmarshal(responseBody, &uukr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal UpdateUserKeyReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(uukr)
		if err != nil {
			return nil, err
		}
	}

	return &uukr, nil
}

// VerifyUpdateUserKey is used to verify a new user identity.
func (c *Client) VerifyUpdateUserKey(vuuk *v1.VerifyUpdateUserKey) (*v1.VerifyUpdateUserKeyReply, error) {
	responseBody, err := c.makeRequest("POST", v1.RouteVerifyUpdateUserKey,
		&vuuk)
	if err != nil {
		return nil, err
	}

	var vuukr v1.VerifyUpdateUserKeyReply
	err = json.Unmarshal(responseBody, &vuukr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal VerifyUpdateUserKeyReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(vuukr)
		if err != nil {
			return nil, err
		}
	}

	return &vuukr, nil
}

// ProposalPaywallPayment retrieves payment details of any pending proposal
// credit payment from the logged in user.
func (c *Client) ProposalPaywallPayment() (*v1.ProposalPaywallPaymentReply, error) {
	responseBody, err := c.makeRequest("GET",
		v1.RouteProposalPaywallPayment, nil)
	if err != nil {
		return nil, err
	}

	var pppr v1.ProposalPaywallPaymentReply
	err = json.Unmarshal(responseBody, &pppr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal ProposalPaywallPaymentReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(pppr)
		if err != nil {
			return nil, err
		}
	}

	return &pppr, nil
}

// UserPaymentsRescan scans the specified user's paywall address and makes sure
// that the user's account has been properly credited with all payments.
func (c *Client) UserPaymentsRescan(upr *v1.UserPaymentsRescan) (*v1.UserPaymentsRescanReply, error) {
	responseBody, err := c.makeRequest("PUT", v1.RouteUserPaymentsRescan, upr)
	if err != nil {
		return nil, err
	}

	var uprr v1.UserPaymentsRescanReply
	err = json.Unmarshal(responseBody, &uprr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal UserPaymentsRescanReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(uprr)
		if err != nil {
			return nil, err
		}
	}

	return &uprr, nil
}

// ProposalsStats retrieves summary statistics for the politeiawww proposal
// inventory.
func (c *Client) ProposalsStats() (*v1.ProposalsStatsReply, error) {
	responseBody, err := c.makeRequest("GET", v1.RoutePropsStats, nil)
	if err != nil {
		return nil, err
	}

	var psr v1.ProposalsStatsReply
	err = json.Unmarshal(responseBody, &psr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal ProposalsStatsReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(psr)
		if err != nil {
			return nil, err
		}
	}

	return &psr, nil
}

// UserProposalCredits retrieves the proposal credit history for the logged
// in user.
func (c *Client) UserProposalCredits() (*v1.UserProposalCreditsReply, error) {
	responseBody, err := c.makeRequest("GET", v1.RouteUserProposalCredits, nil)
	if err != nil {
		return nil, err
	}

	var upcr v1.UserProposalCreditsReply
	err = json.Unmarshal(responseBody, &upcr)
	if err != nil {
		return nil, fmt.Errorf("unmarshal UserProposalCreditsReply: %v", err)
	}

	if c.cfg.Verbose {
		err := prettyPrintJSON(upcr)
		if err != nil {
			return nil, err
		}
	}

	return &upcr, nil
}

// Close all client connections.
func (c *Client) Close() {
	if c.conn != nil {
		c.conn.Close()
	}
}

// New returns a new politeiawww client.
func New(cfg *config.Config) (*Client, error) {
	// Create http client
	tlsConfig := &tls.Config{
		InsecureSkipVerify: cfg.SkipVerify,
	}
	tr := &http.Transport{
		TLSClientConfig: tlsConfig,
	}

	// Set cookies
	jar, err := cookiejar.New(&cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	})
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, err
	}
	jar.SetCookies(u, cfg.Cookies)
	httpClient := &http.Client{
		Transport: tr,
		Jar:       jar,
	}

	return &Client{
		http: httpClient,
		cfg:  cfg,
	}, nil
}
