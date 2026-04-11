#!/bin/bash
# setup-codesign.sh — Create a local code signing certificate for macOS
# This prevents the "would like to access data" popups that plague ad-hoc signed binaries.
# The certificate is stored in login.keychain and persists across rebuilds.

set -e

CERT_NAME="${CODESIGN_IDENTITY:-Soul CLI Dev}"

# Check if we already have a valid codesigning identity
if security find-identity -v -p codesigning 2>/dev/null | grep -q "$CERT_NAME"; then
    echo "$CERT_NAME certificate already exists"
    exit 0
fi

echo "Creating local code signing certificate: $CERT_NAME"
echo "This is a one-time setup to prevent macOS permission popups."
echo ""

TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

# Generate self-signed code signing certificate (valid 10 years)
cat > "$TMPDIR/cert.cfg" <<EOF
[ req ]
default_bits = 2048
prompt = no
default_md = sha256
distinguished_name = dn
x509_extensions = v3_code_sign

[ dn ]
CN = $CERT_NAME

[ v3_code_sign ]
keyUsage = critical, digitalSignature
extendedKeyUsage = codeSigning
basicConstraints = critical, CA:false
EOF

openssl req -x509 -newkey rsa:2048 \
    -keyout "$TMPDIR/key.pem" -out "$TMPDIR/cert.pem" \
    -days 3650 -nodes -config "$TMPDIR/cert.cfg" 2>/dev/null

# Package as PKCS12 (legacy format for macOS compatibility)
openssl pkcs12 -export \
    -out "$TMPDIR/cert.p12" \
    -inkey "$TMPDIR/key.pem" -in "$TMPDIR/cert.pem" \
    -passout pass:setup -legacy 2>/dev/null

# Import to login keychain
security import "$TMPDIR/cert.p12" \
    -k ~/Library/Keychains/login.keychain-db \
    -T /usr/bin/codesign \
    -P "setup" 2>/dev/null

# Trust for code signing (may prompt for system password)
security find-certificate -a -c "$CERT_NAME" -p > "$TMPDIR/trust.pem"
security add-trusted-cert -d -r trustRoot -p codeSign \
    -k ~/Library/Keychains/login.keychain-db "$TMPDIR/trust.pem" 2>/dev/null

# Verify
if security find-identity -v -p codesigning 2>/dev/null | grep -q "$CERT_NAME"; then
    echo "Code signing certificate '$CERT_NAME' created and trusted."
else
    echo "Warning: certificate created but not showing as valid for codesigning."
    echo "You may need to open Keychain Access, find '$CERT_NAME', and set Trust > Code Signing to 'Always Trust'."
    exit 1
fi
