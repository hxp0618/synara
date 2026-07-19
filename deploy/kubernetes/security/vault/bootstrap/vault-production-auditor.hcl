path "auth/token/lookup-self" {
  capabilities = ["read"]
}

path "sys/storage/raft/configuration" {
  capabilities = ["read"]
}

path "__TRANSIT_MOUNT__/keys/__TRANSIT_KEY_NAME__" {
  capabilities = ["read"]
}

path "auth/approle/role/__SIGNER_APPROLE_NAME__" {
  capabilities = ["read"]
}

path "auth/approle/role/__AUDITOR_APPROLE_NAME__" {
  capabilities = ["read"]
}

path "sys/audit" {
  capabilities = ["read", "sudo"]
}

path "sys/policies/acl/__SIGNER_POLICY_NAME__" {
  capabilities = ["read"]
}

path "sys/policies/acl/__AUDITOR_POLICY_NAME__" {
  capabilities = ["read"]
}
