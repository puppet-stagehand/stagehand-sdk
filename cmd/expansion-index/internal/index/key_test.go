package index

import (
	"strings"
	"testing"

	"github.com/puppet-stagehand/stagehand-sdk/cmd/expansion-index/internal/testreg"
)

const fakePrivateKeyBlock = "-----BEGIN EC " + "PRIVATE KEY-----\nc2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0LXNlY3JldC1zZWNyZXQ=\n-----END EC " + "PRIVATE KEY-----\n"

// A key file is parsed, fingerprinted and (by build and listing) published. The
// parser must accept exactly one PUBLIC KEY block, so that nothing it skips can
// be published by a caller that echoes the file.
func TestParsePublicKeyAcceptsOnlyOnePublicKeyBlock(t *testing.T) {
	s := testreg.NewSigner(t)
	for name, ok := range map[string]struct {
		file string
		want bool
	}{
		"plain":              {s.PublicPEM, true},
		"surrounding blanks": {"\n\n" + s.PublicPEM + "\n  \n", true},
		"trailing private":   {s.PublicPEM + fakePrivateKeyBlock, false},
		"leading private":    {fakePrivateKeyBlock + s.PublicPEM, false},
		"trailing text":      {s.PublicPEM + "note\n", false},
		"leading text":       {"note\n" + s.PublicPEM, false},
		"two public keys":    {s.PublicPEM + s.PublicPEM, false},
		"private key only":   {fakePrivateKeyBlock, false},
		"empty":              {"", false},
	} {
		_, err := ParsePublicKey([]byte(ok.file))
		if (err == nil) != ok.want {
			t.Errorf("%s: err=%v, want accepted=%v", name, err, ok.want)
		}
		if err != nil && strings.Contains(err.Error(), "c2VjcmV0") {
			t.Errorf("%s: the error echoes the file's content: %v", name, err)
		}
	}
}

func TestBuildPublishesTheCanonicalKeyInTheIndex(t *testing.T) {
	s := testreg.NewSigner(t)
	k, err := publisherKey([]byte("\n"+s.PublicPEM+"\n\n"), "free")
	if err != nil {
		t.Fatal(err)
	}
	if k.PublicKey != s.PublicPEM {
		t.Fatalf("publisher_keys carries %q, want the canonical encoding %q", k.PublicKey, s.PublicPEM)
	}
}
