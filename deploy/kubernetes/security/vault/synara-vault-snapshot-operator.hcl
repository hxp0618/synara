path "auth/token/lookup-self" {
  capabilities = ["read"]
}

path "sys/storage/raft/configuration" {
  capabilities = ["read"]
}

path "sys/storage/raft/snapshot" {
  capabilities = ["read"]
}

path "transit/keys/synara-worker-release" {
  capabilities = ["read"]
}

path "auth/approle/role/synara-worker-release-signer" {
  capabilities = ["read"]
}

path "auth/approle/role/synara-vault-production-auditor" {
  capabilities = ["read"]
}

path "auth/approle/role/synara-vault-snapshot-operator" {
  capabilities = ["read"]
}

path "sys/policies/acl/synara-worker-release-signer" {
  capabilities = ["read"]
}

path "sys/policies/acl/synara-vault-production-auditor" {
  capabilities = ["read"]
}

path "sys/policies/acl/synara-vault-snapshot-operator" {
  capabilities = ["read"]
}
