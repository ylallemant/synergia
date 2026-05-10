# Apple Developer ID — Code Signing and Notarization

Without a valid Developer ID, macOS Gatekeeper rejects the client binary with
"damaged and can't be opened" when downloaded through a browser.
This document covers everything needed to sign, notarize, and ship the
`synergia-client-darwin-*` binaries so they run without prompts.

---

## Prerequisites

| Requirement | Notes |
|---|---|
| Apple Developer Program membership | $99 / year — enroll at [developer.apple.com/enroll](https://developer.apple.com/enroll) |
| macOS machine for initial certificate creation | Keychain Access is macOS-only |
| Xcode Command Line Tools | `xcode-select --install` |

---

## 1 — Create a Developer ID Application Certificate

1. Open **Keychain Access** → **Keychain Access** menu → **Certificate Assistant** → **Request a Certificate from a Certificate Authority**.
2. Enter your Apple ID e-mail. Select **Saved to disk**. Save the `.certSigningRequest` file.
3. In [developer.apple.com](https://developer.apple.com) → **Certificates, Identifiers & Profiles** → **Certificates** → **+**, choose **Developer ID Application**, upload the `.certSigningRequest`, download the resulting `.cer` file.
4. Double-click the `.cer` to install it into your keychain.

Verify it is installed:

```bash
security find-identity -v -p codesigning | grep "Developer ID Application"
# → 1) XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX "Developer ID Application: Your Name (TEAM_ID)"
```

Note the **10-character Team ID** (shown in parentheses). You will need it later.

---

## 2 — Export the Certificate as a p12 File

The CI pipeline needs the certificate in portable form.

1. Open **Keychain Access** → **My Certificates**.
2. Right-click **Developer ID Application: Your Name (TEAM_ID)** → **Export**.
3. Choose format **Personal Information Exchange (.p12)**; set a strong password. Save as `developer-id.p12`.
4. Base64-encode it for the GitHub secret:

```bash
base64 -i developer-id.p12 | pbcopy   # copies to clipboard
```

---

## 3 — Create an App-Specific Password for Notarytool

Apple's notarization tool (`xcrun notarytool`) requires an app-specific password, not your Apple ID password.

1. Sign in at [appleid.apple.com](https://appleid.apple.com) → **Sign-In and Security** → **App-Specific Passwords** → **+**.
2. Label it `synergia-notarytool` (or similar). Copy the generated password — it is shown only once.

---

## 4 — Add GitHub Actions Secrets

In the repository: **Settings** → **Secrets and variables** → **Actions** → **New repository secret**.

| Secret name | Value |
|---|---|
| `APPLE_DEVELOPER_CERT_P12` | Base64-encoded `.p12` from step 2 |
| `APPLE_DEVELOPER_CERT_PASSWORD` | Password chosen during export |
| `APPLE_ID` | Your Apple ID e-mail address |
| `APPLE_TEAM_ID` | 10-character Team ID from step 1 |
| `APPLE_APP_PASSWORD` | App-specific password from step 3 |

---

## 5 — Release Workflow Integration

Replace the `# TODO: code-sign …` comment in `.github/workflows/release.yml`
with the steps below, inside the `build-client` job, after the **Build binary**
step and before **Upload release asset** — but only for darwin targets:

```yaml
      - name: Import Apple Developer certificate
        if: matrix.goos == 'darwin'
        env:
          P12_BASE64:  ${{ secrets.APPLE_DEVELOPER_CERT_P12 }}
          P12_PASSWORD: ${{ secrets.APPLE_DEVELOPER_CERT_PASSWORD }}
        run: |
          echo "$P12_BASE64" | base64 --decode > /tmp/developer-id.p12
          security create-keychain -p "" build.keychain
          security import /tmp/developer-id.p12 -k build.keychain \
            -P "$P12_PASSWORD" -T /usr/bin/codesign
          security list-keychains -d user -s build.keychain
          security set-key-partition-list -S apple-tool:,apple: -s \
            -k "" build.keychain
        shell: bash

      - name: Sign binary
        if: matrix.goos == 'darwin'
        env:
          TEAM_ID: ${{ secrets.APPLE_TEAM_ID }}
        run: |
          codesign --deep --force --options runtime \
            --sign "Developer ID Application: $(security find-identity -v \
              -p codesigning build.keychain | grep 'Developer ID' | \
              awk -F'"' '{print $2}')" \
            dist/${{ matrix.artifact }}
          codesign --verify --verbose dist/${{ matrix.artifact }}
        shell: bash

      - name: Notarize binary
        if: matrix.goos == 'darwin'
        env:
          APPLE_ID:       ${{ secrets.APPLE_ID }}
          APPLE_TEAM_ID:  ${{ secrets.APPLE_TEAM_ID }}
          APPLE_PASSWORD: ${{ secrets.APPLE_APP_PASSWORD }}
        run: |
          # Notarytool requires a zip for standalone binaries
          ditto -c -k --keepParent dist/${{ matrix.artifact }} \
            dist/${{ matrix.artifact }}.zip
          xcrun notarytool submit dist/${{ matrix.artifact }}.zip \
            --apple-id    "$APPLE_ID" \
            --team-id     "$APPLE_TEAM_ID" \
            --password    "$APPLE_PASSWORD" \
            --wait
          rm dist/${{ matrix.artifact }}.zip
        shell: bash
```

> **Note:** Notarization for standalone CLI binaries does not produce a staple
> ticket — there is nothing to staple to a bare Mach-O binary. Gatekeeper
> performs an online check against Apple's notarization database on first run,
> which requires internet access. This is standard behaviour for signed CLI
> tools distributed outside the App Store.

---

## 6 — Verify Locally

After signing a binary:

```bash
# Check signature
codesign --verify --verbose=4 synergia-client-darwin-arm64

# Check notarization status (after notarytool submit --wait)
spctl --assess --verbose=4 --type exec synergia-client-darwin-arm64
# Expected output: synergia-client-darwin-arm64: accepted
#                  source=Notarized Developer ID
```

---

## 7 — Effect on Users

Once signed and notarized, users can:

- Download via any browser and run directly (no Gatekeeper prompt)
- `chmod +x` is still required (HTTP cannot carry execute permissions)
- The `xattr -dr com.apple.quarantine` workaround in `install.sh` and the
  download page becomes unnecessary but is harmless to keep as a fallback

---

## References

- [Apple: Notarizing macOS software before distribution](https://developer.apple.com/documentation/security/notarizing-macos-software-before-distribution)
- [Apple: Code Signing Guide](https://developer.apple.com/library/archive/documentation/Security/Conceptual/CodeSigningGuide/)
- [xcrun notarytool man page](https://keith.github.io/xcode-man-pages/notarytool.1.html)
