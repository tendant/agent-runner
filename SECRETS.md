# Secrets

Environment configuration lives in two files:

| file | contents | Git |
|---|---|---|
| `.env` | local plaintext working copy | never committed |
| `.env.sops` | ordinary config readable, secret values encrypted | committed |

`.sops.yaml` records the age recipients allowed to decrypt, and the policy that
only values carrying a `# sops:encrypt` comment get encrypted. Everything else
stays readable so configuration changes remain reviewable in a diff.

## After cloning

```sh
make secrets-decrypt
```

You need an authorized age private identity. On Linux SOPS finds it at
`~/.config/sops/age/keys.txt`; on macOS its default location is
`~/Library/Application Support/sops/age/keys.txt`, so these scripts resolve
either one and export `SOPS_AGE_KEY_FILE` for you.

## Changing configuration or secrets

Edit `.env` as usual, then:

```sh
make secrets-encrypt
git diff -- .env.sops     # config changes readable, secrets stay ENC[...]
git add .env.sops && git commit
```

To add a new secret, put a `# sops:encrypt` comment on the line above it:

```sh
PAYMENTS_ENDPOINT=https://payments.example.com

# sops:encrypt
PAYMENTS_API_KEY=secret-value
```

Mark secrets explicitly rather than trusting the variable name. A name-based
rule misses `DATABASE_URL=postgres://user:password@host/db`, and over-matches
on values like `STRIPE_PUBLISHABLE_KEY` that are public by design.

## Changing who can decrypt

`make secrets-encrypt` refuses to run if `.sops.yaml` lists a recipient the
existing `.env.sops` does not already have. Editing secrets can never silently
expand access. To actually change access, add or remove the recipient in
`.sops.yaml` and run:

```sh
make secrets-authorize     # interactive; shows the diff and requires "yes"
```

Removing a recipient only controls future access. It cannot un-read a secret
someone already decrypted, so rotate the underlying credential too.

## Checking

```sh
make secrets-check         # read-only; safe in CI
```

Verifies the file is valid SOPS dotenv, that configured recipients match the
ones actually baked into the file, that every marked value is encrypted, and
that `.env` is not tracked by Git.
