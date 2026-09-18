package auth

import "golang.org/x/crypto/bcrypt"

// HashPassword hashes a plaintext password with bcrypt at the library's
// default cost. It is the only place in this package that should ever touch
// a plaintext password's bytes directly.
func HashPassword(plaintext string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

// ComparePassword reports whether plaintext matches the given bcrypt hash.
func ComparePassword(hash, plaintext string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)) == nil
}

// timingEqualizerHash is compared against on a login miss (unknown email, or
// an account with no password hash) so that path takes about as long as a
// genuine wrong-password comparison. Without it, response time alone tells an
// attacker which emails are registered.
var timingEqualizerHash = func() []byte {
	hash, err := bcrypt.GenerateFromPassword([]byte("auth-timing-equalizer"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return hash
}()

// burnTimingEqualizer runs a bcrypt comparison that always fails, spending
// about the same time a real comparison would have. The result is
// deliberately discarded — this call exists for its cost, not its outcome.
func burnTimingEqualizer(plaintext string) {
	_ = bcrypt.CompareHashAndPassword(timingEqualizerHash, []byte(plaintext))
}
