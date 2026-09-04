package jmsd

import (
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// verifyTOTP checks code against secret using JumpServer's own parameters:
// apps/users/utils.py:78-83's check_otp_code calls pyotp.TOTP(otp_secret_key)
// with no overrides, i.e. SHA1/6 digits/30s step -- pquerna/otp's constants
// match.
//
// The acceptance window deliberately does NOT match. JumpServer passes
// valid_window=settings.OTP_VALID_WINDOW, which defaults to 2
// (apps/jumpserver/conf.py:551) and so accepts five 30-second windows;
// Skew of 1 accepts three. Narrower is the safe direction -- it is the
// numerator of the guessing odds the MFA lockout is sized against (see
// mfaLockout) -- and one window either side already covers real clock drift.
// The cost is that a code JumpServer's own web login would still take can be
// refused here once drift passes 45 seconds, which is a clock to fix, not a
// window to widen.
func verifyTOTP(secret, code string) bool {
	if secret == "" || code == "" {
		return false
	}
	ok, _ := totp.ValidateCustom(code, secret, time.Now(), totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	return ok
}
