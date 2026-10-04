package stack

import (
	"crypto/x509"
	"errors"
	"fmt"
	"testing"
)

func TestOtherStackReason(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("stack peer certificate: %w", x509.UnknownAuthorityError{}),
		errors.New("remote error: tls: bad certificate"),
		errors.New("remote error: tls: unknown certificate authority"),
	} {
		if got := otherStackReason(err); got != "its certificate is from another stack" {
			t.Errorf("%v: %q", err, got)
		}
	}
	if got := otherStackReason(errors.New("i/o timeout")); got != "i/o timeout" {
		t.Errorf("other errors are kept: %q", got)
	}
	p := &vcPort{}
	if !p.noteOtherStack("a x") || p.noteOtherStack("a x") || !p.noteOtherStack("b x") || p.noteOtherStack("") || !p.noteOtherStack("b x") {
		t.Fatal("a neighbour of another stack must be logged once per neighbour and reason")
	}
}
