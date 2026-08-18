package shell

// Test seams. Both are nil in every build except the concurrency test.
//
// The window this file exists for — between a shell deciding to create an
// instance and claiming the project — is a few microseconds wide. A race test
// that merely starts goroutines and hopes they collide passes whether or not
// the bug is present, which is the same as not testing it. These let the test
// hold every racing shell at that exact point and release them together.
var (
	// hookAfterDecideCreate runs after a shell decides to create an instance,
	// before it claims the project.
	hookAfterDecideCreate func()

	// hookBeforeNegotiate runs immediately before a shell probes host ports.
	// Counting these is what distinguishes the fixed ordering (exactly one
	// shell ever negotiates) from the broken one (every racing shell does, and
	// all but the winner prompt about a conflict they created themselves).
	hookBeforeNegotiate func()
)
