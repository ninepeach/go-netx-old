//go:build netxdebug

package buf

import "testing"

// Checks: BUF-17
func TestReleasePoisonsInDebug(t *testing.T) {
	p := NewPool()
	b := p.Get(100)
	b.WriteString("secret data from the last owner")
	stale := b.data // a reference kept past Release (a bug)
	b.Release()
	for i, v := range stale {
		if v != Poison {
			t.Fatalf("byte %d = %#x after Release, want Poison %#x", i, v, Poison)
		}
	}
	// The next owner sees Poison, not the previous owner's data, in any
	// byte it does not write itself.
	c := p.Get(100)
	defer c.Release()
	if fresh := c.FreeBytes(); fresh[0] != Poison && &fresh[0] == &stale[0] {
		t.Fatal("reused buffer still holds old data")
	}
}
