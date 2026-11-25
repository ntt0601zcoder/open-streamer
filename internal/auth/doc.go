// Package auth holds the API's users, their PBKDF2-SHA-256 password hashes
// and the failed-login limiter.
//
// A user is a name, a role (viewer reads, editor reads and writes) and a
// hash in the form pbkdf2-sha256$<iterations>$<salt>$<key>, salt and key
// in unpadded standard base64. The package starts no goroutine.
//
// A caller checking HTTP Basic credentials follows this order:
//
//  1. Limiter.Blocked(key): refuse before anything is compared, so a
//     blocked client never learns whether a guess equals a remembered
//     password.
//  2. Users.Remembered: success with no PBKDF2; call Limiter.Succeed(key)
//     and take no reservation.
//  3. Limiter.Reserve(key): refuse when it is false.
//  4. Users.Verify, then Attempt.Succeed, Attempt.Fail on
//     ErrBadCredentials, or Attempt.Abandon on ErrBusy and on a ctx error.
package auth
