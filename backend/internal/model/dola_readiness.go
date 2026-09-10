package model

const DolaReadinessVersion = "protocol-30s-v1"

// A usable credential is not proof that its browser can submit a video.
func DolaAccountReady(account TokenAccount) bool {
	generation, _ := account.Meta["dola_credential_generation"].(string)
	verified, _ := account.Meta["dola_verified_generation"].(string)
	version, _ := account.Meta["dola_readiness_version"].(string)
	state, _ := account.Meta["dola_readiness"].(string)
	return account.Pool == "dola" && state == "ready" && version == DolaReadinessVersion && generation != "" && generation == verified
}
