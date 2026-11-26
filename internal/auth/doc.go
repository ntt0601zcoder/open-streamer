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
//  3. Limiter.Reserve(key, remoteAddr): refuse when it is false; otherwise
//     defer attempt.Abandon() at once, so every exit resolves the attempt.
//     Besides the key's place it takes one of the 4 places of the client
//     address, held until the attempt is resolved, so across Users.Verify
//     and its queue: one address inventing names has at most 4
//     verifications in flight. An address is never blocked for failures.
//  4. Users.Verify, then Attempt.Succeed, or Attempt.Fail on
//     ErrBadCredentials; the deferred Abandon covers ErrBusy, a ctx error
//     and a panic, and does nothing after Succeed or Fail.
package auth
