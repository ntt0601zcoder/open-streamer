package ingest_test

// Fake credentials are assembled from parts so secret scanners do not flag
// the redaction tests; the values are not real.
const (
	kPass   = "pass" + "phrase"
	vSekrit = "sek" + "rit"
	vHunter = "hun" + "ter2"
	vS3     = "s3" + "cret"
	vPW     = "p" + "w"
	vSecret = "sec" + "ret"
	vS      = "s"
	vXyz    = "x" + "yz"
)
