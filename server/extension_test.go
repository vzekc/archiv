package archiv

import "context"

// The tests run with a Messenger, so that the catalog's notifications are
// queued.
type testMessenger struct{}

func (testMessenger) Init(*App) error                                    { return nil }
func (testMessenger) Send(context.Context, string, string, string) error { return nil }

func init() {
	extensions = []Extension{testMessenger{}}
	pbkdf2Rounds = 1000 // fast hashes for the tests
}
