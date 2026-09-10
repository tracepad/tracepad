package store

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// Passwords (spec 028 Decision 1). `bcrypt` is the boring choice, and the cost
// is stated here rather than taken from the library's default so that a change
// in either is a change in the diff.
//
// The rule is a length and nothing else: composition rules ("one digit, one
// symbol") measurably make passwords worse, and the ceiling is where `bcrypt`
// stops reading the input anyway.
const (
	// PasswordCost is the bcrypt work factor. Twelve is about a quarter of
	// a second on the hardware this runs on, which is the point: a login is
	// one request a person makes twice a month, and a dictionary run is
	// millions.
	PasswordCost = 12

	// MinPasswordLength and MaxPasswordLength bound what may be set.
	MinPasswordLength = 10
	MaxPasswordLength = 128
)

// ErrPasswordLength is a password outside the bounds above. The message is
// what the person setting it reads, so it says both ends.
var ErrPasswordLength = fmt.Errorf(
	"a password must be between %d and %d characters", MinPasswordLength, MaxPasswordLength)

// HashPassword checks the length and hashes. The two are one call because a
// caller that hashed first and validated afterwards would have spent a quarter
// of a second on a password it was going to refuse.
func HashPassword(password string) ([]byte, error) {
	if len(password) < MinPasswordLength || len(password) > MaxPasswordLength {
		return nil, ErrPasswordLength
	}
	return bcrypt.GenerateFromPassword([]byte(password), PasswordCost)
}

// Verify reports whether the password is this account's.
//
// An account with no password — invited and not yet accepted — verifies
// nothing, which is what makes `pending` answer the login with the same 401 as
// a wrong password (Decision 8).
func (a *Account) Verify(password string) bool {
	if a == nil || len(a.hash) == 0 {
		return false
	}
	// Any error is a refusal, mismatch or unreadable hash alike: there is
	// nothing a caller could do with the difference, and saying which it
	// was would say whether the account has a password at all.
	return bcrypt.CompareHashAndPassword(a.hash, []byte(password)) == nil
}
