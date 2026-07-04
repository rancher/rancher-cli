package token

import "time"

type typeMeta struct {
	Kind       string `json:"kind,omitempty"`
	APIVersion string `json:"apiVersion,omitempty"`
}

type execCredentialStatus struct {
	ExpirationTimestamp *time.Time `json:"expirationTimestamp,omitempty"`
	Token               string     `json:"token,omitempty"`
}

type execCredential struct {
	typeMeta
	Status *execCredentialStatus `json:"status,omitempty"`
}

func newExecCredential(token string, expiry time.Time) *execCredential {
	c := &execCredential{
		typeMeta: typeMeta{Kind: "ExecCredential", APIVersion: "client.authentication.k8s.io/v1beta1"},
		Status:   &execCredentialStatus{Token: token},
	}
	if !expiry.IsZero() {
		c.Status.ExpirationTimestamp = &expiry
	}
	return c
}
