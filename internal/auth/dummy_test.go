package auth

// resetDummyForTest forgets the per-process dummy hash, so the next use
// makes it again.
func resetDummyForTest() { dummyCell.Store(&dummyBox{}) }
