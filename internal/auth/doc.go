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
//  2. Users.Remembered: success with no PBKDF2 and no reservation. The
//     remembered path touches no limiter state: only a real verification
//     (Attempt.Succeed) zeroes a key's failures.
//  3. Limiter.Reserve(key): refuse when it is false; otherwise defer
//     attempt.Abandon() at once, so every exit resolves the attempt.
//  4. Users.Verify, then Attempt.Succeed, or Attempt.Fail on
//     ErrBadCredentials; the deferred Abandon covers ErrBusy, a ctx error
//     and a panic, and does nothing after Succeed or Fail.
package auth
