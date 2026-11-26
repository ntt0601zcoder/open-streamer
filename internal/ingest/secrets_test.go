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

// vS3Key is a fake stream key that redacts to "s3xxxxx", and vP4ss a fake
// password.
const (
	vS3Key = vS3 + "key"
	vP4ss  = "p4" + "ss"
)

// vStreamKey is a fake RTMP stream key.
const vStreamKey = "sk" + "_9f3a7c" + "e21b"
