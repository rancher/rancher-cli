// Package access answers authorization questions about Rancher-managed
// clusters with the server's own authorizer: SubjectAccessReview,
// SelfSubjectAccessReview, SelfSubjectRulesReview, and SelfSubjectReview
// posted through the Rancher proxy. Nothing here evaluates a Role's rules
// client-side; the package classifies server answers, resolves subjects,
// resources, and scopes, and renders what the server said.
package access
