package identity

import "testing"

// These vectors freeze the persisted pre-refactor length-delimited SHA-256
// protocol. A package move must not reopen old operations under different keys.
func TestPersistedFingerprintCompatibility(t *testing.T) {
	for _, test := range []struct {
		fields []string
		want   string
	}{
		{nil, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{[]string{"a|b", "c"}, "06f4aee86c37742da9a45a4e0e291c7391ad9de261c419c45114c93931de4d44"},
		{[]string{"a", ""}, "6aa98e17c109dd8e2ae23a478ceb48e193c730289ba48d741388a3cc8b38ef4f"},
		{[]string{"provider-message-v1", "paddle", "merchant", "sandbox", "evt_1"}, "fdda81f5fb2a7d6d2b5468a933777a94fc9dfc527b3d0529dc3ddd1848c3aaca"},
	} {
		if got := Fingerprint(test.fields...); got != test.want {
			t.Fatalf("%q: got %s want %s", test.fields, got, test.want)
		}
	}
}
