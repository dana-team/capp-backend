# Desired state and drift

With `desiredState.enabled`, the backend records the desired state of every Capp,
ConfigMap, Secret and namespace it manages in MongoDB. When a client reads an object,
the backend compares it with its record and reports the differences. The client then
decides what to do with them.

There is no watcher and no periodic job. Drift that nobody reads is not noticed, and
nothing is changed without a request.

Why it works this way is in [desired-state-design.md](desired-state-design.md). If you used
Git sync, start with [Moving off Git sync](#moving-off-git-sync).

## What is recorded

| Kind | Recorded | Not recorded |
|---|---|---|
| Capp | `spec`, labels, annotations | status, server metadata, and what the operator writes: `last-updated-by`, `skip-dns-check`, `rcs.dana.io/rollout`, the restart annotations |
| ConfigMap | `data`, `binaryData`, `immutable`, labels, annotations | server metadata |
| Secret | `type`, `data` (encrypted), `immutable`, labels, annotations | server metadata |
| Namespace | quota limits, and its users or backend-policy bindings | everything else |

- Only ConfigMaps and Secrets with `dana.io/capp-managed=true`, and namespaces with
  `dana.io/capp-ns=true`, take part.
- Labels and annotations are compared only for the keys in the record. Controllers add
  their own, and those are not drift.
- `kubectl.kubernetes.io/*` annotations and the `rcs.dana.io/backup-to-git` label, a leftover
  of Git sync, are ignored.
- cfunc pools and functions are not tracked.

## Writes

The cluster is changed first. Then the object the cluster returned is recorded, so
defaults never look like drift and a write the cluster refuses is never recorded.

| Situation | Result |
|---|---|
| MongoDB is down, or the record cannot be read | The change is made and not recorded. Success with a `DESIRED_STATE_FAILED` warning; a later read shows drift |
| The cluster refuses the write | The cluster's error, nothing recorded |
| MongoDB fails after the cluster write | Same as when it is down |
| MongoDB is down and the request asks for the desired state: revert, accept, adopt, `onDrift=revert` | `503 DESIRED_STATE_FAILED`, nothing changed |

Deleting an object removes its record. Deleting a namespace removes all of its records.
A migration records everything it creates, so drift of the source becomes the target's
desired state. A rollback replaces the whole spec and does not ask about drift.

## Drift states

| State | Meaning | Revert | Accept |
|---|---|---|---|
| `inSync` | Cluster and record match | – | – |
| `changed` | Both exist and differ | Update the cluster | Record the live object |
| `missing` | Recorded, but gone from the cluster | Recreate it | Archive the record |
| `unmanaged` | Exists, but has no record | – | Record it |
| `unavailable` | The record could not be read or compared | – | – |

Reads return the object's `drift`. Lists also return `missing`. Reading a missing object
answers 404 with `details.drift`. Secrets report only the paths that differ, never values.

Revert and accept send back the `generation` and `liveVersion` they were shown. If either
changed in the meantime, the request answers `409 DRIFT_DETECTED` with the current drift.

- **Revert** uses the caller's credential and needs the permission of the equivalent
  update. Reverting a ConfigMap or Secret restarts the Capps that opted in.
- **Accept and adopt** change only MongoDB and need no more than a read. In backend-policy
  mode that includes viewers. Accepting replaces the recorded state, and the previous one
  is not kept. Accepting a `missing` object keeps its record as archived.

## Without MongoDB

Desired state is a backup, and nothing else depends on it.

- **Not enabled:** responses carry no `drift`, and the drift endpoints answer 501. Everything else
  works as it did before the feature existed.
- **Enabled, but MongoDB is down or slow:** the backend starts and serves every request. Reads
  report `unavailable`. Writes are made, not recorded, and carry a warning. Updates cannot check
  for drift, so they do not ask about it.
- **Back again:** what was not recorded shows as drift (`changed`, `missing` or `unmanaged`), and
  is settled like any other drift.

After a timeout the backend leaves MongoDB alone for 10 seconds, so that an outage does not delay
every request. Invalid `desiredState` settings still stop the startup, like any other invalid
setting.

## Updating an object that has drifted

`PUT` on a Capp, ConfigMap, Secret or namespace, `PATCH` on a namespace and removing a
rollout strategy answer `409 DRIFT_DETECTED` when the object has drifted. Otherwise an
update would silently record changes that somebody made directly in the cluster.

- `?onDrift=accept` starts from the live object.
- `?onDrift=revert` starts from the desired state. On a namespace `PATCH` it also needs the
  permission that `PUT` needs, because it resets the quota and removes users.
- `&liveVersion=…&generation=…` limit the decision to the drift that was shown, taken from the
  409. If the object or its desired state changed since, the request answers 409 again with the
  current drift. Without them, `onDrift` applies to whatever drift there is, as
  `cappctl --on-drift` intends. The console always sends them.

Fields in the request always win. A client that fills a form from the live object must
load the desired values before it sends `onDrift=revert`. `cappctl update --on-drift revert`
does this: it reverts first and then reads the Capp again.

## Enabling it

1. **Provide MongoDB.** A standalone server works. Run a replica set and back it up if you
   rely on it to restore Secrets.
2. **Create an encryption key** and keep it somewhere safe. Secrets cannot be read without it.

   ```bash
   echo "k1=$(openssl rand -base64 32)"
   ```

3. **Set the Helm values:**

   ```yaml
   config:
     desiredState:
       enabled: true
       encryption:
         activeKeyID: k1
   secret:
     desiredStateMongoURI: "mongodb://user:password@mongo:27017/?authSource=capp"
     desiredStateEncryptionKeys: "k1=<base64>"
   ```

4. **Adopt what already exists.** Until then, existing objects are `unmanaged` and have no
   backup. Run this once per namespace:

   ```bash
   cappctl drift adopt --cluster <name> --namespace <namespace>
   ```

   Adopting takes the cluster as it is, including changes made by hand. It never replaces
   an existing record and can be repeated. Namespaces themselves are recorded by accepting
   them or by their next update.

In Kubernetes authorization mode, the cluster credential needs `delete` on
`resourcequotas` to revert a quota that was added by hand. The chart grants it in
backend-policy mode.

## Moving off Git sync

Git sync was removed. Desired state replaces it as the backup. These are gone:

- `POST` and `DELETE` on `.../capps/:name/sync`, `cappctl sync`, and the MCP tools `capp_sync`
  and `capp_sync_disable`
- the `GITOPS_FAILED` error and warning code, and `gitOpsPath` in cluster responses
- the `gitops` configuration, `clusters[].gitOpsPath` and the `helm/capp-template` chart

Leftover Git sync settings are ignored. When `gitops.enabled` is still true, the backend logs a
warning at startup and Helm prints one, because **Capps are then no longer backed up to Git**.
Without `desiredState` they are not backed up at all.

> **Detach Argo CD before you upgrade, and do not run `cappctl sync --disable` first.**
> `--disable` deletes the values file. Depending on the setup, Argo CD then deletes the Capp.

1. **Detach Argo CD and keep the Capps.** Until this is done, Argo CD keeps applying the last
   values in Git and undoes every later change, including a revert.
   - If an ApplicationSet creates the Applications, delete it first so that it does not
     recreate them: `kubectl delete applicationset <name> --cascade=orphan`
   - Delete each Application without its resources: `argocd app delete <name> --cascade=false`
   - Check that the Capps still exist.
2. **Remove the settings:** `config.gitops`, `config.clusters[].gitOpsPath`,
   `secret.gitopsToken`, `secret.sshKey`, and any `CAPP_GITOPS_*` variables.
3. **Upgrade with `desiredState.enabled`** as described in [Enabling it](#enabling-it).
4. **Adopt every namespace.** The repository is your only backup until you have. Keep it,
   read-only, until you have checked that the objects are recorded.
5. **Optionally remove the leftover label.** Nothing reads it. The operator records a
   CappRevision for each Capp you change.

   ```bash
   kubectl label capps.rcs.dana.io -A -l rcs.dana.io/backup-to-git rcs.dana.io/backup-to-git-
   ```

## Rotating the encryption key

Add the new key next to the old one, then make it active. Every Secret is encrypted with
the active key the next time it is recorded. Keep the old key until no record uses it:

```js
db.objects.countDocuments({ "desired.secretData.keyId": "k1" })
```

Rotating changes every `liveVersion`, so a revert or accept in flight answers 409 once.
