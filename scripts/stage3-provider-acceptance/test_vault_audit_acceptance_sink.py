from __future__ import annotations

import contextlib
import hashlib
import http.client
import json
import pathlib
import ssl
import subprocess
import sys
import tempfile
import threading
import types
import unittest
import urllib.parse
from unittest import mock
from typing import Any

SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import vault_audit_acceptance_sink as sink


class FakeObjectLockClient:
    def __init__(self, *, retention_days: int = 45) -> None:
        self.options = types.SimpleNamespace(bucket="synara-vault-audit", prefix="entries")
        self.uploads: dict[str, bytes] = {}
        self.retention_days = retention_days

    def verify_bucket_contract(self) -> Any:
        return sink.object_lock.BucketContract(
            versioning_status="Enabled",
            default_retention_mode="COMPLIANCE",
            default_retention_days=self.retention_days,
        )

    def qualify_object_key(self, basename: str) -> str:
        return f"entries/{basename}"

    def upload_bytes(self, object_key: str, content: bytes) -> str:
        self.uploads[object_key] = content
        return hashlib.sha256(content).hexdigest()

    def verify_existing_object(
        self,
        object_key: str,
        *,
        expected_content_sha256: str,
        version_id: str | None = None,
    ) -> Any:
        content = self.uploads[object_key]
        actual = hashlib.sha256(content).hexdigest()
        if actual != expected_content_sha256:
            raise sink.object_lock.S3ObjectLockError(
                "s3_object_lock.content_hash_drift",
                "fixture content drift",
            )
        return sink.object_lock.ObjectVersionEvidence(
            object_key=object_key,
            version_id=version_id or "fixture-version-id",
            etag="fixture-etag",
            content_sha256=actual,
            retain_until="2099-01-01T00:00:00Z",
            retention_mode="COMPLIANCE",
        )

    def cat_version(self, object_key: str, _version_id: str) -> bytes:
        return self.uploads[object_key]

    def probe_delete_version(self, _object_key: str, _version_id: str) -> Any:
        return sink.object_lock.NegativeProbeResult(
            operation="deleteVersion",
            blocked=True,
            return_code=1,
            statuses=("error",),
            error_codes=("ObjectLocked",),
        )

    def probe_shorten_retention(self, _object_key: str, _version_id: str) -> Any:
        return sink.object_lock.NegativeProbeResult(
            operation="shortenRetention",
            blocked=True,
            return_code=1,
            statuses=("error",),
            error_codes=("ObjectLocked",),
        )


CA_CERT_PEM = """-----BEGIN CERTIFICATE-----
MIIDDzCCAfegAwIBAgIUQBPhd3ZOseMqHk+S8GYiAZCc1wUwDQYJKoZIhvcNAQEL
BQAwFzEVMBMGA1UEAwwMVGVzdCBSb290IENBMB4XDTI2MDcxOTE3NDc1NVoXDTM2
MDcxNjE3NDc1NVowFzEVMBMGA1UEAwwMVGVzdCBSb290IENBMIIBIjANBgkqhkiG
9w0BAQEFAAOCAQ8AMIIBCgKCAQEAx1jzGwaUmc+j7K7in9wq/WLLXo6vytXodx4t
7XFd7yf1jOqbSD9IKLpN8B66TGxkfN6q9Sc+Zu8g0UidXMIHW8Ofo3mKKX6V0U8Z
3Qf1bl66Ffmp1w6ejfW60wIzYNBeNgEGXDqX4c52K7z+kZSLPY3oSeyRMU6Y7H6I
V9isze5E1nOtgsOFiAxLa1MDHe0XstPBoYT4GzByz0DdODz6I5/aE1ge12D99OWN
zLdNxTbjIOBYIXlv6Z6QjhOTHs0TgTiAD5AwD7PYg8fzRO1PVN+doIDzOs41ZPs6
qJ1RUUgtaMdm8621RmlCco24QbIg0nxjJ3eq+Rsp3QII5J0TiwIDAQABo1MwUTAd
BgNVHQ4EFgQU6qDrhdNoQqUJnbgn/te1W4h+oE8wHwYDVR0jBBgwFoAU6qDrhdNo
QqUJnbgn/te1W4h+oE8wDwYDVR0TAQH/BAUwAwEB/zANBgkqhkiG9w0BAQsFAAOC
AQEAb5ZpgkUElsybe+UVhgOyycVIPm/BlnugJEo+6G84w/d3ssdNqtWStJIRlE6+
u2daaaSiqP+vyAcay1nGZXP+GboJYQ0apSk+1TQyP0pAQeOKu5QEdkEBzNCOwNZB
1oo48CNmp9vB5oxBTRRjD9vXrsgYhOCAtxvBG2Hid8NjLnwLtrRz97H5I/P8SoxP
+WlkPKL6++UDcctmEo4pR3kfKbGSv7RBpVj7COsp6cWASu6bKUvaYA8JcQ3Jy847
cpNKV3CwyXHPBeF+YlPFdNSQu14P6qBbu+TKDJY0d7mEZ4I8LvNaYyfGHYEu2avv
R/JxDgfiv41aS2uxCN51Ml/zFg==
-----END CERTIFICATE-----
"""

SERVER_CERT_PEM = """-----BEGIN CERTIFICATE-----
MIIDNjCCAh6gAwIBAgIUeXYFqoszZpJNrETQwrTdtkyiyEMwDQYJKoZIhvcNAQEL
BQAwFzEVMBMGA1UEAwwMVGVzdCBSb290IENBMB4XDTI2MDcxOTE3NDc1NVoXDTM2
MDcxNjE3NDc1NVowFDESMBAGA1UEAwwJbG9jYWxob3N0MIIBIjANBgkqhkiG9w0B
AQEFAAOCAQ8AMIIBCgKCAQEA3hdaRknOLVKqALyv0CArKlmGdM/rCt4CEBJC0lf/
SW51oZUBDCq5AILDSIo3y0iFNnkyubu+Qk3VXs6AjVtO/BuGRkaf8zQ54e5M/Ous
MAM8Pj4SvVcwJGiFfEwvxmger1SG3gi2PBpQOusWmbSSOR5g2vfhohPdtROmq/Mo
NO6Jfv0nxv5xUuurBXCBjIq1QOCABfcaCjzEo2odzFRNqIItLR52zA1mVQmozUAO
D69tqX6EURwNb7O3lUP5ef+p9JZgttFUFzsviLeKozq21b67QpSHzoz9qK/pcEOs
1lydnLigQZS3XkeiETgZwAFu5nJzOWRhLM5RkIhDhYCHpQIDAQABo30wezAaBgNV
HREEEzARgglsb2NhbGhvc3SHBH8AAAEwHQYDVR0lBBYwFAYIKwYBBQUHAwEGCCsG
AQUFBwMCMB0GA1UdDgQWBBTE6TJfYRlYu6qmkPaYl2u2sIGQQTAfBgNVHSMEGDAW
gBTqoOuF02hCpQmduCf+17VbiH6gTzANBgkqhkiG9w0BAQsFAAOCAQEAwE/p5zCQ
qOrivk85/jcY3wX7DeopP3ovPZDabsdv5Nmv78vGYCLRyPtZssuwVTYbWR/iGleH
pchD8WrJglClOpB8cXL2eZaNUZKZ6t069LMvD5aPum6SqUPbwU9ZpxHEcGxD1uq3
0WRmUeu+cRc7IhmSoCn/YP4k7aY+oXZHLibRHmWkoCSdN7MVhcZ2zjzMszmH1pAe
QbfnbdoabOQhc7BB29kNaagyCvDcY79Mz2xSS3PfcAyGRO7MJJ63ePkZG2Uaxq9m
tGKDt8851XpatIDDvXIIrZyY8v72cU7FZWlhIs5i89vHE+fqxgzTnUEH7dNKO7rs
viLrcMhdtFhufA==
-----END CERTIFICATE-----
"""

SERVER_KEY_PEM = """-----BEGIN PRIVATE KEY-----
MIIEvAIBADANBgkqhkiG9w0BAQEFAASCBKYwggSiAgEAAoIBAQDeF1pGSc4tUqoA
vK/QICsqWYZ0z+sK3gIQEkLSV/9JbnWhlQEMKrkAgsNIijfLSIU2eTK5u75CTdVe
zoCNW078G4ZGRp/zNDnh7kz866wwAzw+PhK9VzAkaIV8TC/GaB6vVIbeCLY8GlA6
6xaZtJI5HmDa9+GiE921E6ar8yg07ol+/SfG/nFS66sFcIGMirVA4IAF9xoKPMSj
ah3MVE2ogi0tHnbMDWZVCajNQA4Pr22pfoRRHA1vs7eVQ/l5/6n0lmC20VQXOy+I
t4qjOrbVvrtClIfOjP2or+lwQ6zWXJ2cuKBBlLdeR6IROBnAAW7mcnM5ZGEszlGQ
iEOFgIelAgMBAAECgf8CbdiO7D+7cl82b6av00PYzFUSi5oGhGca+RgoaAEvmTjj
WFd3ZDNumsxUy5SdcWDQaulWUeqPm+PsvyJCaYoNN9lzUbjUib2x7hyDDhDUDzT/
363uZDjvYDVIwFlfBV4dWZwgBMUsr+nKVMfqgA8ZXaIX9je0wU+reCUnVefneRWb
clteoJad3dLEq98XxcYnZDSVwIQbFWpCoZjXAzPAfsF2/f95MTGkNTDvM6PjT/0A
ycQ4g9TmJ+zp8GuGXGaU5rSUIVM+cogDuxKL+ccl8mRLl+jf2PXYl7q5qyyiZVGC
nC92GCER4/3cKR97D0ObZgP3AaoJfy2KUke8lwkCgYEA+8PeHNjb1ug2QHX8fPu7
PMYt3Qz6/i8LypzUuWSueDe2UYSUgF6DbfzGokAFYiKYV97CsQmT/6Bbu++X5b1x
rnOMoqRcwmsiBnGHWJ9CC0E/nkP98muJTo1fKphfzpi3nhJUu6n+Pm/WUrQKMrgF
8ZNxNMihjvflxnLjjpN0K18CgYEA4dO0m7i4uvf652I5h9Xt7OuRybO8BEjvEXAQ
JwtY2LELKHZx8G5xh47250S8/yhG+PF1dwk6HtB6ULINjeLd7ne4pHzrFAoJI2zx
Yci2aWSEmSSD2FSlFEQuaupawomEvv6kS8cpoMgQQZhmUPdJrhZs5lx3BRPnLEtr
+McO73sCgYEAw0NlaEA1ORe+w/3+Rr1Cud8GwTQJEs1QOuOqBOP2gRzMlarbNjiX
fN2Y/UvkIPmt6DDIFWDVXWR04WzxBWkJ24CY6afKnatTp2Wz0GMsaOhBPDGFqtgG
lVsGHVYysFw3xSx4dVhh7PD2bAxhAHdDfNqa6ZJV4zmXB3Qh03m/lscCgYBPZ6Vl
6/nopDFxErSv8qUKXXqRtcUyrIKDWygS0oaXCwmlXKCLrgn1ZGukviLGhV8PQbfP
90qccynPHgxuC4uFwksGa3YtQaoc7r2haHXbcSC+yHwjoP+6tI6twWHQbZJjph4X
FxyoEDDHH9M6PPmHYRNBnNmsy2bJyGtauoOh6QKBgQDS5reBURam2jDg3443oYCS
9hqdfk285MjV/KleU+TsucJMqfQLKIqr/dU1Y/jOstOshI8w0bKU53ViFsCCtsop
kGuVbPD+MviOKv07bA53LCbv310cdX0HLfSSJ9ix9e0me2cFtDtL3Yyx2YgaNPSB
OCELagNgLZYCYAH8goONjw==
-----END PRIVATE KEY-----
"""

CLIENT_CERT_PEM = """-----BEGIN CERTIFICATE-----
MIIDGTCCAgGgAwIBAgIUeXYFqoszZpJNrETQwrTdtkyiyEQwDQYJKoZIhvcNAQEL
BQAwFzEVMBMGA1UEAwwMVGVzdCBSb290IENBMB4XDTI2MDcxOTE3NDc1NVoXDTM2
MDcxNjE3NDc1NVowHTEbMBkGA1UEAwwSdmF1bHQtYXVkaXQtY2xpZW50MIIBIjAN
BgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAxFtaUL+KGtMtSNQCxq8YHmrKYASu
SUFhEeJC3Ho+seLbXAdFZ59hLYWKVOKDj8Tk4KFz703quHtCVJ7ZlD+SleFwSB0v
v5Fb/vwHLQkR3Kh9pyCZ9BonmVtcZSxpJQxiNbvd3wqrWkVexfAo2YUPO5VURgq8
yQBwBNSQhyF5PD0vPcbihSfLP9+CY+9BToCnbO80cVQ4daexKmxpOy3da3keGKlE
rgtVQmP8ppgntub6FH103i46gHxTXVymb6/l370qqhpPJeiM0zLqZY/NbMCc5Glv
nEHbwecY0Tue1Az64HbHZTqxxtR0b/rRCYQ22HdgmXm9kOJHWSlSVLzpLQIDAQAB
o1cwVTATBgNVHSUEDDAKBggrBgEFBQcDAjAdBgNVHQ4EFgQUv2axHuQumwVIb/9X
8jKJ3hiGnpAwHwYDVR0jBBgwFoAU6qDrhdNoQqUJnbgn/te1W4h+oE8wDQYJKoZI
hvcNAQELBQADggEBAE1WEuDF9FdyXv01q8Hga6hmRNxDs1N8bngQfo+nt8SZGARq
jWYNyr5nkz4Uc1iN6vF4CxeHXv/Mn/UzzJgR/ulpPpeTcLu2GdkM/PilfYAz17pm
iiaJe3prWmtK2GgazkHmb8/7wrmgO9FaGd2sVLRIrfXXJJ6nzdPkJo22m40/fC4W
j7HJKMj7+pSw/OAXrljPqKf/S854VqoeiPWyx6yvC7ZXqXltv/CRo/yssy0sQ1Ef
a3iT5PpBVPTI3piTh8DFlG7lmk2bE3lxPa0gUtHvFwDkn+Mk2JFrVno88kOk2RLY
Qbswq5eUEQa2fzMYLGr6UeUYqS5HtNzVOujC2d4=
-----END CERTIFICATE-----
"""

CLIENT_KEY_PEM = """-----BEGIN PRIVATE KEY-----
MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQDEW1pQv4oa0y1I
1ALGrxgeaspgBK5JQWER4kLcej6x4ttcB0Vnn2EthYpU4oOPxOTgoXPvTeq4e0JU
ntmUP5KV4XBIHS+/kVv+/ActCRHcqH2nIJn0GieZW1xlLGklDGI1u93fCqtaRV7F
8CjZhQ87lVRGCrzJAHAE1JCHIXk8PS89xuKFJ8s/34Jj70FOgKds7zRxVDh1p7Eq
bGk7Ld1reR4YqUSuC1VCY/ymmCe25voUfXTeLjqAfFNdXKZvr+XfvSqqGk8l6IzT
Muplj81swJzkaW+cQdvB5xjRO57UDPrgdsdlOrHG1HRv+tEJhDbYd2CZeb2Q4kdZ
KVJUvOktAgMBAAECggEAJh2smb2gr6XcJcW/9zUAe9ETkVF/SomWL/xJqdZSCsJk
fggMTTnnSYihamphCvy3yDIXGPY5UM/ed6IxTsGLcSxGmT0PKoLrRoNCWlfnH3wW
jiV6NSQJmU+ejyYwj+hIPTGHd3cw/ZA3PjmpGFZnt1N8vS7y6Bq9Y/amSpDxIYPQ
9zdDYs1jqPO9YNSPUifwhRpiUoIwHT0rZph8Q/I8FYQ2Bm8bXmMAN/8iKw7EnfL0
N3gpHKpcFLDVXOmWp2eBVaO/DL8WqQZyZ8yBvnCStkf91GFCf7yzi/oP0epjsCXj
hZ3zIxpSrYDTiVf1c1wNWoEIY424tRINqp1lXozNeQKBgQDgiCPbr8/YhP+Hualr
GARUaOaGbZFlzrU/H/VBJcIKU1VE6yxcfFICzY3Dd3JZ1cBq/bBRKd9tRRDH407V
2FqomYt1ASDx71SyBpkL6DlyvlTyGhxWebbQ55EJdx+huC1NpwPILIzxbP60k0dG
gojXkbjNk9uVmJGFLHUF6O/pSQKBgQDf4FaaOmadExN8yMAXU5p66KX+MSX2IrFf
+FjS7KgwIMWbPHJcimYLv8sBqnuhYhUYBxpR1bZs3KmbsOPh05+UO594yb8lI1ug
mTf8Hy92o+HP63eV2O8jfWvFkkdNKZssqxwPdvNAKSWxP9euYU0wJajRhbVpwFP2
nepGF+9ExQKBgQCSuSQFiSvfJ3n77V1CeF1L84jAy5S53IwgBfg0bEISkUYlVTCV
9z94SW6cDtAQ2Fd3EvRG9X/lXb6LgIShxVHo3v18phIrRuQnuZwFZek0jB/iXSGr
eLn4ZXonn0pyWXJxTfRwuHwZv8npolxvPRnDFJyY6kgRx7NAPT7zb7Zm0QKBgCf1
EV/rhn8IdZTy+53uNQc02NOakAzzOjdHywqyZH5aiwpe6oZryTTVoXUFqZUvPVaR
hfgPLcUWSUtZcgLPU48QaTEUyQHm4qayUhS0uDLzow0KGMjs9BmgfAjCR+mUwHZj
f9mewGG2Nl0BaQxdn3o1boEe3TcntZSxsKub//+FAoGBAJnOSPw8vfExRaOb2LwF
+q0+VIecgsXGKCho22bztDmOjxtuyWSo9wcFi5kqFMOeHdRI+L0ffH7oe6rauiSR
9geFqhO+1F+ExfRSVNwY0gRYR/HXxZgq0M5WMdrqrlHoU8TS9aiLV44FgQoFvkNj
0bnn4eoZ82P42NhmVezFuWPR
-----END PRIVATE KEY-----
"""


def write_tls_material(root: pathlib.Path) -> dict[str, pathlib.Path]:
    ca_conf = root / "ca.cnf"
    server_conf = root / "server.cnf"
    client_conf = root / "client.cnf"
    ca_conf.write_text(
        "\n".join(
            [
                "[ req ]",
                "default_bits = 2048",
                "prompt = no",
                "default_md = sha256",
                "distinguished_name = dn",
                "x509_extensions = v3_ca",
                "[ dn ]",
                "CN = Test Root CA",
                "[ v3_ca ]",
                "subjectKeyIdentifier = hash",
                "authorityKeyIdentifier = keyid:always,issuer",
                "basicConstraints = critical, CA:true",
                "keyUsage = critical, keyCertSign, cRLSign",
            ]
        )
        + "\n",
        encoding="utf-8",
    )
    server_conf.write_text(
        "\n".join(
            [
                "[ req ]",
                "default_bits = 2048",
                "prompt = no",
                "default_md = sha256",
                "distinguished_name = dn",
                "req_extensions = req_ext",
                "[ dn ]",
                "CN = localhost",
                "[ req_ext ]",
                "subjectAltName = @alt_names",
                "extendedKeyUsage = serverAuth",
                "keyUsage = critical, digitalSignature, keyEncipherment",
                "[ alt_names ]",
                "DNS.1 = localhost",
                "IP.1 = 127.0.0.1",
            ]
        )
        + "\n",
        encoding="utf-8",
    )
    client_conf.write_text(
        "\n".join(
            [
                "[ req ]",
                "default_bits = 2048",
                "prompt = no",
                "default_md = sha256",
                "distinguished_name = dn",
                "req_extensions = req_ext",
                "[ dn ]",
                "CN = vault-audit-client",
                "[ req_ext ]",
                "extendedKeyUsage = clientAuth",
                "keyUsage = critical, digitalSignature, keyEncipherment",
            ]
        )
        + "\n",
        encoding="utf-8",
    )
    paths = {
        "ca_cert": root / "ca.crt",
        "ca_key": root / "ca.key",
        "server_cert": root / "server.crt",
        "server_key": root / "server.key",
        "server_csr": root / "server.csr",
        "client_cert": root / "client.crt",
        "client_key": root / "client.key",
        "client_csr": root / "client.csr",
    }
    subprocess.run(
        [
            "openssl",
            "req",
            "-x509",
            "-newkey",
            "rsa:2048",
            "-days",
            "3650",
            "-nodes",
            "-config",
            str(ca_conf),
            "-keyout",
            str(paths["ca_key"]),
            "-out",
            str(paths["ca_cert"]),
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    subprocess.run(
        [
            "openssl",
            "req",
            "-newkey",
            "rsa:2048",
            "-nodes",
            "-keyout",
            str(paths["server_key"]),
            "-out",
            str(paths["server_csr"]),
            "-config",
            str(server_conf),
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    subprocess.run(
        [
            "openssl",
            "x509",
            "-req",
            "-in",
            str(paths["server_csr"]),
            "-CA",
            str(paths["ca_cert"]),
            "-CAkey",
            str(paths["ca_key"]),
            "-CAcreateserial",
            "-out",
            str(paths["server_cert"]),
            "-days",
            "3650",
            "-sha256",
            "-extfile",
            str(server_conf),
            "-extensions",
            "req_ext",
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    subprocess.run(
        [
            "openssl",
            "req",
            "-newkey",
            "rsa:2048",
            "-nodes",
            "-keyout",
            str(paths["client_key"]),
            "-out",
            str(paths["client_csr"]),
            "-config",
            str(client_conf),
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    subprocess.run(
        [
            "openssl",
            "x509",
            "-req",
            "-in",
            str(paths["client_csr"]),
            "-CA",
            str(paths["ca_cert"]),
            "-CAkey",
            str(paths["ca_key"]),
            "-CAcreateserial",
            "-out",
            str(paths["client_cert"]),
            "-days",
            "3650",
            "-sha256",
            "-extfile",
            str(client_conf),
            "-extensions",
            "req_ext",
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    return paths


def certificate_sha256(pem_text: str) -> str:
    return sink.sha256_bytes(ssl.PEM_cert_to_DER_cert(pem_text))


def https_json_request(
    base_url: str,
    path: str,
    *,
    method: str = "GET",
    ca_cert: pathlib.Path,
    client_cert: pathlib.Path | None = None,
    client_key: pathlib.Path | None = None,
    body: str | None = None,
) -> tuple[int, Any]:
    parsed = urllib.parse.urlsplit(base_url)
    context = ssl.create_default_context(cafile=str(ca_cert))
    if client_cert is not None and client_key is not None:
        context.load_cert_chain(certfile=str(client_cert), keyfile=str(client_key))
    connection = http.client.HTTPSConnection(parsed.hostname, parsed.port, context=context, timeout=5.0)
    try:
        headers = {"Accept": "application/json"}
        payload = body.encode("utf-8") if body is not None else None
        if payload is not None:
            headers["Content-Type"] = "application/x-ndjson"
            headers["Content-Length"] = str(len(payload))
        connection.request(method, path, body=payload, headers=headers)
        response = connection.getresponse()
        raw = response.read()
    finally:
        connection.close()
    return response.status, json.loads(raw) if raw else None


@contextlib.contextmanager
def running_sink(
    retention_days: int = 30,
    *,
    object_lock_client: FakeObjectLockClient | None = None,
) -> Any:
    with tempfile.TemporaryDirectory() as directory:
        root = pathlib.Path(directory)
        tls_paths = write_tls_material(root)
        state_dir = root / "state"
        options = sink.SinkOptions(
            bind_host="127.0.0.1",
            port=0,
            state_dir=state_dir,
            server_cert_path=tls_paths["server_cert"],
            server_key_path=tls_paths["server_key"],
            client_ca_cert_path=tls_paths["ca_cert"],
            retention_days=retention_days,
            object_lock_environment_names=("VAULT_AUDIT_WORM_MC_HOST",)
            if object_lock_client is not None
            else (),
        )
        server = sink.create_server(options, object_lock_client=object_lock_client)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            host, port = server.server_address
            yield {
                "server": server,
                "thread": thread,
                "base_url": f"https://{host}:{port}",
                "tls": tls_paths,
                "state_dir": state_dir,
            }
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5.0)


def request_event(request_id: str) -> dict[str, object]:
    return {
        "host": "synara-vault-0",
        "file": "/vault/audit/audit-primary.log",
        "offset": 0,
        "vault_audit": {
            "time": "2026-07-19T17:48:00Z",
            "type": "request",
            "request": {
                "id": request_id,
                "operation": "read",
                "path": "sys/audit",
            },
            "namespace": {"id": "root"},
        },
    }


def response_event(request_id: str) -> dict[str, object]:
    return {
        "host": "synara-vault-0",
        "file": "/vault/audit/audit-primary.log",
        "offset": 256,
        "vault_audit": {
            "time": "2026-07-19T17:48:01Z",
            "type": "response",
            "request": {
                "id": request_id,
                "operation": "read",
                "path": "sys/audit",
            },
            "namespace": {"id": "root"},
        },
    }


class SinkIntegrationTest(unittest.TestCase):
    def test_accepts_mtls_ndjson_and_exposes_receipt_chain_retention(self) -> None:
        with running_sink(retention_days=45) as fixture:
            request_id = "req-accept-001"
            body = "\n".join(
                json.dumps(item)
                for item in (request_event(request_id), response_event(request_id))
            )

            status, payload = https_json_request(
                fixture["base_url"],
                "/v1/audit/events",
                method="POST",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
                body=body,
            )

            self.assertEqual(status, 202)
            self.assertEqual(payload["accepted"], 2)
            self.assertEqual(payload["receipts"][0]["audit"]["requestId"], request_id)
            self.assertEqual(
                fixture["state_dir"].stat().st_mode & 0o777,
                0o700,
            )

            status, health = https_json_request(
                fixture["base_url"],
                "/healthz",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
            )
            self.assertEqual(status, 200)
            self.assertEqual(health["ledger"]["entryCount"], 2)
            self.assertTrue(health["ledger"]["verified"])

            status, receipt = https_json_request(
                fixture["base_url"],
                f"/v1/receipts?request_id={request_id}&path=sys/audit",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
            )
            self.assertEqual(status, 200)
            self.assertEqual(receipt["receipt"]["audit"]["operation"], "read")
            self.assertEqual(
                receipt["receipt"]["transport"]["peerCertificateSha256"],
                certificate_sha256(fixture["tls"]["client_cert"].read_text(encoding="utf-8")),
            )

            status, chain = https_json_request(
                fixture["base_url"],
                "/v1/chain",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
            )
            self.assertEqual(status, 200)
            self.assertTrue(chain["verified"])
            self.assertEqual(chain["entryCount"], 2)

            status, retention = https_json_request(
                fixture["base_url"],
                "/v1/retention",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
            )
            self.assertEqual(status, 200)
            self.assertFalse(retention["policy"]["immutable"])
            self.assertFalse(retention["policy"]["storageEnforced"])
            self.assertEqual(retention["policy"]["retentionDays"], 45)
            self.assertTrue(retention["policy"]["earliestExpiry"])

            status, delete_payload = https_json_request(
                fixture["base_url"],
                f"/v1/receipts?request_id={request_id}&path=sys/audit",
                method="DELETE",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
            )
            self.assertEqual(status, 405)
            self.assertEqual(delete_payload["error"]["code"], "sink.method_not_allowed")

    def test_archives_exact_batch_to_storage_enforced_compliance_object(self) -> None:
        archive = FakeObjectLockClient()
        with running_sink(retention_days=45, object_lock_client=archive) as fixture:
            request_id = "req-object-lock-001"
            status, payload = https_json_request(
                fixture["base_url"],
                "/v1/audit/events",
                method="POST",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
                body=json.dumps(request_event(request_id)),
            )
            self.assertEqual(status, 202)
            receipt = payload["receipts"][0]
            object_key = receipt["archive"]["objectKey"]
            archived = archive.uploads[object_key]
            self.assertEqual(receipt["archive"]["batchEntryCount"], 1)
            self.assertEqual(receipt["archive"]["batchContentSha256"], hashlib.sha256(archived).hexdigest())
            archived_entries = [
                json.loads(line)
                for raw_line in archived.decode("utf-8").splitlines()
                if (line := raw_line.strip())
            ]
            self.assertEqual(len(archived_entries), 1)
            archived_entry = archived_entries[0]
            self.assertEqual(archived_entry["payload"], request_event(request_id)["vault_audit"])
            self.assertEqual(
                archived_entry["payloadSha256"],
                sink.sha256_bytes(sink.stable_json_bytes(archived_entry["payload"])),
            )
            self.assertEqual(receipt["payloadSha256"], archived_entry["payloadSha256"])
            self.assertEqual(
                archived_entry["entrySha256"],
                sink._canonical_entry_hash(archived_entry),
            )
            self.assertEqual(archived_entry["entrySha256"], receipt["entrySha256"])
            self.assertNotIn("payload", receipt)

            status, retention = https_json_request(
                fixture["base_url"],
                "/v1/retention",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
            )
            self.assertEqual(status, 200)
            self.assertTrue(retention["policy"]["immutable"])
            self.assertTrue(retention["policy"]["storageEnforced"])
            self.assertEqual(retention["objectLock"]["mode"], "COMPLIANCE")

    def test_rejects_missing_mtls(self) -> None:
        with running_sink() as fixture:
            with self.assertRaises((ssl.SSLError, OSError)):
                https_json_request(
                    fixture["base_url"],
                    "/healthz",
                    ca_cert=fixture["tls"]["ca_cert"],
                )

    def test_rejects_duplicate_sequence_and_missing_receipt(self) -> None:
        with running_sink() as fixture:
            request_id = "req-dup-001"
            first = json.dumps(request_event(request_id))
            status, _ = https_json_request(
                fixture["base_url"],
                "/v1/audit/events",
                method="POST",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
                body=first,
            )
            self.assertEqual(status, 202)

            duplicate = dict(request_event("req-dup-002"))
            duplicate["offset"] = 0
            status, payload = https_json_request(
                fixture["base_url"],
                "/v1/audit/events",
                method="POST",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
                body=json.dumps(duplicate),
            )
            self.assertEqual(status, 409)
            self.assertEqual(payload["error"]["code"], "sink.sequence_invalid")

            status, missing = https_json_request(
                fixture["base_url"],
                "/v1/receipts?request_id=missing-id&path=sys/audit",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
            )
            self.assertEqual(status, 404)
            self.assertEqual(missing["status"], "not_found")

    def test_accepts_offset_reset_only_as_a_bounded_rotation_generation(self) -> None:
        with running_sink() as fixture:
            before_rotation = request_event("req-before-rotation-001")
            before_rotation["offset"] = sink.ROTATION_MIN_PRIOR_SEQUENCE
            status, first = https_json_request(
                fixture["base_url"],
                "/v1/audit/events",
                method="POST",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
                body=json.dumps(before_rotation),
            )
            self.assertEqual(status, 202)
            self.assertEqual(first["receipts"][0]["stream"]["generation"], 0)

            after_rotation = request_event("req-after-rotation-001")
            after_rotation["offset"] = 0
            after_rotation["vault_audit"]["time"] = "2026-07-19T17:49:00Z"
            status, second = https_json_request(
                fixture["base_url"],
                "/v1/audit/events",
                method="POST",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
                body=json.dumps(after_rotation),
            )
            self.assertEqual(status, 202)
            self.assertEqual(second["receipts"][0]["stream"]["generation"], 1)

            status, chain = https_json_request(
                fixture["base_url"],
                "/v1/chain",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
            )
            self.assertEqual(status, 200)
            self.assertTrue(chain["verified"])
            self.assertEqual(chain["entryCount"], 2)

    def test_detects_tampered_chain(self) -> None:
        with running_sink() as fixture:
            request_id = "req-chain-001"
            body = "\n".join(
                json.dumps(item)
                for item in (request_event(request_id), response_event(request_id))
            )
            status, _ = https_json_request(
                fixture["base_url"],
                "/v1/audit/events",
                method="POST",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
                body=body,
            )
            self.assertEqual(status, 202)

            ledger_path = fixture["state_dir"] / sink.LEDGER_FILE_NAME
            lines = ledger_path.read_text(encoding="utf-8").splitlines()
            tampered = json.loads(lines[-1])
            tampered["payloadSha256"] = "0" * 64
            lines[-1] = json.dumps(tampered, sort_keys=True, separators=(",", ":"))
            ledger_path.write_text("\n".join(lines) + "\n", encoding="utf-8")

            status, chain = https_json_request(
                fixture["base_url"],
                "/v1/chain",
                ca_cert=fixture["tls"]["ca_cert"],
                client_cert=fixture["tls"]["client_cert"],
                client_key=fixture["tls"]["client_key"],
            )
            self.assertEqual(status, 409)
            self.assertFalse(chain["verified"])
            self.assertIn(
                "sink.chain_payload_hash_mismatch",
                {error["code"] for error in chain["errors"]},
            )
            self.assertIn(
                "sink.chain_hash_mismatch",
                {error["code"] for error in chain["errors"]},
            )


class AuditLedgerStateTest(unittest.TestCase):
    def _build_options(self, root: pathlib.Path) -> sink.SinkOptions:
        tls_paths = write_tls_material(root)
        return sink.SinkOptions(
            bind_host="127.0.0.1",
            port=0,
            state_dir=root / "state",
            server_cert_path=tls_paths["server_cert"],
            server_key_path=tls_paths["server_key"],
            client_ca_cert_path=tls_paths["ca_cert"],
        )

    def _transport(self) -> dict[str, Any]:
        return {
            "mutualTlsVerified": True,
            "clientAddress": "127.0.0.1",
            "tlsVersion": "TLSv1.3",
            "cipherSuite": "TLS_AES_256_GCM_SHA384",
            "peerCertificateSha256": "a" * 64,
            "peerChainSha256": ["b" * 64],
            "peerSubject": "CN=test-client",
            "peerIssuer": "CN=test-ca",
            "peerSerialNumber": "01",
        }

    def test_reuses_cached_verification_and_receipt_lookup_does_not_reserialize_history(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            options = self._build_options(root)
            first = request_event("req-cache-001")
            second = request_event("req-cache-002")
            second["offset"] = 512
            second["vault_audit"]["time"] = "2026-07-19T17:49:00Z"
            verify_calls = 0
            original_verify = sink.verify_ledger_chain

            def counted_verify(ledger_path: pathlib.Path) -> sink.ChainVerification:
                nonlocal verify_calls
                verify_calls += 1
                return original_verify(ledger_path)

            with mock.patch.object(sink, "verify_ledger_chain", side_effect=counted_verify):
                ledger = sink.AuditLedger(options)
                self.assertEqual(verify_calls, 1)

                ledger.append_batch([first], transport=self._transport())
                ledger.append_batch([second], transport=self._transport())
                self.assertEqual(verify_calls, 1)

                with mock.patch.object(
                    sink,
                    "stable_json_bytes",
                    side_effect=AssertionError("receipt lookup should not serialize full history"),
                ):
                    receipt = ledger.receipt("req-cache-001", "sys/audit")

                self.assertEqual(receipt["audit"]["requestId"], "req-cache-001")
                self.assertEqual(verify_calls, 1)

    def test_fails_closed_when_ledger_changes_after_start(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            options = self._build_options(root)
            ledger = sink.AuditLedger(options)
            ledger.append_batch([request_event("req-runtime-drift-001")], transport=self._transport())

            ledger_path = root / "state" / sink.LEDGER_FILE_NAME
            original = ledger_path.read_text(encoding="utf-8")
            ledger_path.write_text(f"{original}\n", encoding="utf-8")

            with self.assertRaises(sink.SinkError) as context:
                ledger.receipt("req-runtime-drift-001", "sys/audit")
            self.assertEqual(context.exception.code, "sink.state_invalid")

            status, chain = ledger.chain_report()
            self.assertEqual(status, 409)
            self.assertIn(
                "sink.chain_runtime_drift",
                {error["code"] for error in chain["errors"]},
            )


if __name__ == "__main__":
    unittest.main()
