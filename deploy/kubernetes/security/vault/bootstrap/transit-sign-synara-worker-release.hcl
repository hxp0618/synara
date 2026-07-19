path "auth/token/lookup-self" {
  capabilities = ["read"]
}

path "__TRANSIT_MOUNT__/keys/__TRANSIT_KEY_NAME__" {
  capabilities = ["read"]
}

path "__TRANSIT_MOUNT__/sign/__TRANSIT_KEY_NAME__" {
  capabilities = ["update"]
}

path "__TRANSIT_MOUNT__/sign/__TRANSIT_KEY_NAME__/*" {
  capabilities = ["update"]
}
