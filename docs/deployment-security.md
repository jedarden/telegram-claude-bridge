# Deployment security validation

The proxy/bridge split is a security boundary: only the proxy may receive the
Telegram bot token. The bridge receives a token-free HTTP API over Tailscale.
`scripts/validate-deployment-security.sh` checks that boundary without changing
cluster or Tailscale state.

The validator has two modes:

- `--static-only` checks the bridge systemd unit in this repository.
- The full form checks the proxy Deployment and Service, the complete proxy
  manifest directory, and the Tailscale ACL policy. It can also compare a
  token supplied on stdin against the bridge process tree and an explicitly
  named bridge deployment root.

The full check intentionally requires source and destination identities as
arguments. Tailscale tags are infrastructure decisions and must not be guessed
from stale application documentation.

## Static deployment check

Run the repository-local check with:

```bash
make validate-deployment-security
```

For a deployed proxy, obtain read-only copies of the exact manifests and ACL
policy from their source-of-truth repositories, then run:

```bash
scripts/validate-deployment-security.sh \
  --proxy-manifest-dir /path/to/telegram-bridge-manifests \
  --acl-policy /path/to/policy.hujson \
  --tailscale-source tag:codinghome \
  --tailscale-destination tag:telegram-proxy:8080 \
  --bridge-unit deploy/telegram-claude-bridge.service
```

The checks fail if any of the following drift occurs:

- the bridge unit receives `BOT_TOKEN`, `TELEGRAM_TOKEN`, OpenBao/Vault
  credentials, an `EnvironmentFile`, or a direct Telegram endpoint;
- the proxy token is a literal manifest value, is injected anywhere except the
  proxy container, or is not supplied through a Kubernetes secret reference;
- the proxy is exposed by `LoadBalancer`, `NodePort`, host networking/ports,
  an ingress, or an unpinned `:latest` image;
- the proxy Service is not a Tailscale-exposed `ClusterIP` at `:8080`;
- the ACL lacks the exact TCP allow rule and executable ACL test, or grants the
  declared source wildcard access that includes the proxy destination.

## Runtime token check

To prove that the token is absent from the live bridge environment, descendant
process arguments, and bridge files, pipe it directly from the secret manager.
Do not put it in a shell variable, command argument, log, or report. The
validator stores the piped value only in a temporary mode-600 file and removes
it on exit.

Example shape (replace the secret-manager command and path with the deployment's
approved read-only command):

```bash
approved-secret-read-command-that-writes-only-to-stdout |
  scripts/validate-deployment-security.sh \
    --proxy-manifest-dir /path/to/telegram-bridge-manifests \
    --acl-policy /path/to/policy.hujson \
    --tailscale-source tag:codinghome \
    --tailscale-destination tag:telegram-proxy:8080 \
    --bridge-unit deploy/telegram-claude-bridge.service \
    --token-stdin \
    --bridge-pid "$(systemctl --user show telegram-claude-bridge.service -p MainPID --value)" \
    --bridge-root /home/coding/.telegram-claude-bridge-deploy
```

The runtime scan is deliberately scoped to the exact deployment root. It
rejects broad roots such as `/`, `/home`, `/root`, `/etc`, `/var`, and `/tmp`,
and it never prints the token or matching content. A disappearing descendant is
allowed, but failure to inspect the bridge process itself fails the check.

The validator is read-only. Kubernetes desired state remains owned by the
declarative-config repository, and Tailscale ACL changes must continue through
that repository's normal GitOps/Terraform flow.
