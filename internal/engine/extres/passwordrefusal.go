package extres

import (
	"errors"
	"net/http"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// invalidCurrentPasswordText is the text of the 500 response to a
// PasswordChangeRequest create request when a user without permission to
// change every user's password changes their own password and the current
// password is wrong or missing. Only Rancher's password check returns
// "invalid current password", and only that create request wraps it in
// "error updating password: ". Rancher v2.13 to v2.16 send this text; a
// response with any other text keeps the code it has on every command.
const invalidCurrentPasswordText = "error updating password: invalid current password"

// notAuthorizedText is the text of the 401 response to a PasswordChangeRequest
// create request when a user changes another user's password without
// permission to change every user's password. Rancher's code for that create
// request returns this text in one place only. Rancher v2.13 to v2.16 send it;
// a response with any other text keeps the code it has on every command.
const notAuthorizedText = "not authorized to update password"

// refusePasswordChange maps the two refusals that Rancher sends in response to
// a PasswordChangeRequest create request with the status and code of an
// unrelated failure: a refused current password (a 500, which classifies as
// GENERIC) becomes VALIDATION, and a refusal for another user's password (a
// 401, which classifies as AUTH) becomes RBAC. sentCurrent says whether the
// request carried a current password: Rancher responds to a wrong and to a
// missing current password with the same text; the message follows what the
// command sent and never claims more than Rancher's own words. A match needs
// the text, the code and the status together; every other error is returned
// unchanged. The new error has no hint, is not retryable, and holds none of
// the submitted passwords. The server's error stays in the cause.
func refusePasswordChange(err error, sentCurrent bool, userID string) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok {
		return err
	}
	status, ok := clierr.HTTPStatus(err)
	if !ok {
		return err
	}
	switch {
	case status == http.StatusInternalServerError && ce.Code == clierr.CodeGeneric && strings.Contains(ce.Message, invalidCurrentPasswordText):
		if sentCurrent {
			return clierr.New(clierr.CodeValidation,
				"Rancher refused the current password (invalid current password); the password was not changed.").WithCause(err)
		}
		return clierr.New(clierr.CodeValidation,
			"Rancher requires the current password to change your own password; "+
				"pipe the current password and the new password, one per line, and pass --current-password-stdin with --password-stdin. "+
				"The password was not changed.").WithCause(err)
	case status == http.StatusUnauthorized && ce.Code == clierr.CodeAuth && strings.Contains(ce.Message, notAuthorizedText):
		return clierr.New(clierr.CodeRBAC,
			"you can change only your own password; to change the password of user "+clierr.IDText(userID)+
				" you need permission to update users.management.cattle.io and secrets in the cattle-local-user-passwords namespace").WithCause(err)
	}
	return err
}
