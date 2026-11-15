package ingest

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
	vRight  = "right" + kPass
	vWrong  = "wrong" + kPass
	vXyz    = "x" + "yz"
)

// vStreamKey is a fake RTMP stream key.
const vStreamKey = "sk" + "_9f3a7c" + "e21b"
