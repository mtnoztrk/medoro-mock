# medoro-mock

A stand-in for the Medoro / ipsp.lv payment gateway, for **UAT only**. Medoro has no sandbox, so
this app plays the gateway: it receives the backend's encrypted payment form, shows a payment page
where the tester picks an outcome (success or a failure reason), then sends a real, encrypted and
signed callback to the UAT backend about 1.5 seconds later — exactly the flow production uses.

No environment variables. Port `2727` inside the container, keys in `keys/mock/`, scenarios in
`scenarios.json`.

## How it maps to the real flow

| Production | Mock |
|---|---|
| `POST https://ipsp.lv/form/v2/` (auto-submit form with `INTERFACE`, `KEY`, `DATA`, `SIGNATURE`, `CALLBACK`, `ERROR_CALLBACK`) | `POST /form/v2/` on this app |
| Medoro card entry + 3DS pages | One page with the order summary and a scenario picker |
| Browser posts encrypted `DATA`/`KEY`/`SIGNATURE` to `CALLBACK`; backend redirects the browser back to the site | Same, after a ~1.5 s "Processing payment" screen |
| Medoro server-to-server notification | Optional checkbox; posts the same triple to `.../medoro-server-callback` |

The crypto is byte-compatible with `MedoroService`: AES-256-ECB with PKCS7 padding for `DATA`,
RSA PKCS1v15 for the AES key wrap, SHA-256 PKCS1v15 for the signature.

## Keys — the written record

Two **test** PEM files sit in the root of this repo, and the same two files sit in the
`WeddingScam/` project directory of the backend repo:

- `medoro_merchant.pem` — the test merchant **private** key.
- `medoro_gateway.pem` — the test gateway **private** key. The real production file with this
  name holds only the public key; the test file holds the full private key because the mock must
  sign callbacks with it. The backend only uses the public half, so the same file works there.

**The rule: the two files must stay byte-identical in both repos.** There is no automation for
this, on purpose. The keys never expire and never rotate, so the only way to break the pairing is
to regenerate one side and forget the other. If you ever regenerate, copy both files to both repos
in the same change. A mismatch fails loudly: the mock logs an RSA key-mismatch error on the
payment request, and the backend throws `SIGNATURE_MISMATCH` on the callback.

These test keys are public by design and guard nothing. Production never uses them: the prod
image build overwrites both files with the real PEMs, and the prod readiness check refuses a
container that still holds the test merchant key.

## Run

```bash
docker compose up -d --build     # listens on host port 2727
```

Or locally: `go run .` (listens on `:2727`).

The mock must be reachable **from the tester's browser**, because both the payment form post and
the callback post travel through the browser. Expose it through nginx, for example:

```nginx
server {
    server_name medoro-mock.example.com;   # your UAT hostname for the mock
    location / { proxy_pass http://127.0.0.1:2727; }
    # plus the usual TLS block
}
```

## Point the UAT backend at the mock

`MedoroSettings` in `appsettings.UAT.json` (WeddingScam repo) overrides the production URLs:

```json
"MedoroSettings": {
    "ApiUrl": "https://medoro-mock.example.com/form/v2/",
    "RedirectUrlBase": "https://uat-backend.example.com/api/payments/medoro-redirect?id=",
    "CallbackUrl": "https://uat-backend.example.com/api/payments/medoro-callback",
    "ErrorCallbackUrl": "https://uat-backend.example.com/api/payments/medoro-callback",
    "SuccessRedirectUrl": "https://uat.example.com/people"
}
```

The mock reads the callback URL from the request's `CALLBACK` field, so it needs no configuration
of its own.

## Scenarios

`scenarios.json` holds the outcomes offered on the payment page. Each entry is:

```json
{
    "id": "fail-cvv",
    "label": "Failure — CVV2 declined",
    "kind": "failure",
    "description": "State 4, ActionCode 187. The payment is rejected.",
    "xml": "<?xml version=\"1.0\" ...?><data>...</data>"
}
```

`xml` is a Go `text/template`. Placeholders filled from the incoming payment:
`{{.PaymentID}}`, `{{.OrderID}}`, `{{.Amount}}`, `{{.Currency}}`, `{{.Description}}`,
`{{.Name}}`, `{{.Email}}`, `{{.StartDate}}`, `{{.LastDate}}`.

To add a scenario: take a real Medoro callback XML, replace the order-specific values with the
placeholders, escape the double quotes for JSON, and append the entry. The file is read on every
request (and volume-mounted in docker-compose), so no rebuild is needed.

Backend state mapping for reference: `6` completed, `2`/`4`/`5` rejected, `3` pending, `8` refunded.
Any other state (for example `7`, seen in production with ActionCode `000`) falls back to rejected.
Repeated callbacks are safe — the backend locks the payment row and ignores a callback for an
already-completed payment.

## Manual mode

Open `/` in a browser to fire a callback without going through the app: enter the payment
transaction reference (order GUID), the amount in minor units and the callback URL, then pick a
scenario. Useful for replaying a callback for an existing pending transaction.

## Endpoints

| Endpoint | Purpose |
|---|---|
| `GET /` | Manual start form + loaded scenario list |
| `POST /form/v2/` | Gateway entry point (drop-in for ipsp.lv) |
| `GET /pay?id=` | Payment page with scenario picker |
| `POST /confirm` | Builds the callback, shows the processing screen |
| `GET /healthz` | Liveness |
