path "auth/token/lookup-self" {
  capabilities = ["read"]
}

path "transit/keys/synara-worker-release" {
  capabilities = ["read"]
}

path "transit/sign/synara-worker-release" {
  capabilities = ["update"]
}

path "transit/sign/synara-worker-release/*" {
  capabilities = ["update"]
}
