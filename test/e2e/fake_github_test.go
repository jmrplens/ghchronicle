package e2e

import (
	"testing"

	"github.com/jmrplens/ghchronicle/test/e2e/fakegh"
)

// The fake GitHub these tests collect from lives in test/e2e/fakegh, so the
// containerized suite one directory down can import the same route table
// rather than keep a second copy of it. What is left here is the two names
// this package spells without a qualifier.

type recorded = fakegh.Request

const login = fakegh.Login

func newFakeGitHub(t *testing.T) *fakegh.Server {
	t.Helper()
	return fakegh.New(t, "testdata")
}
