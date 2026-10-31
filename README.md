# local-path-exporter

Mesure l’usage disque réel de chaque PVC `local-path` sur k3s. Les volumes vivent dans `/var/lib/rancher/k3s/storage` (`pvc-<id>_<namespace>_<pvc>`) sans plafond : une PVC demandée à 2 Gio peut occuper des dizaines de Gio. `KubePersistentVolumeFillingUp` ne voit que le disque du nœud.

DaemonSet, un pod par nœud. Pas de driver CSI, pas de `hostNetwork`. Seul le répertoire de stockage est monté, en lecture seule. Un scan tourne en tâche de fond (défaut 5 min) ; `/metrics` sert le cache.

## Métriques

Labels des séries volume : `namespace`, `persistentvolumeclaim`, `persistentvolume`, `node`. Les deux premiers sont ceux du kubelet ; `persistentvolume` est le nom du PV, ou le préfixe `pvc-<id>` si le répertoire est lu sans PV. `node` est `NODE_NAME`. Aucune valeur chiffrée n’est mise dans un label. Les séries sont reconstruites à chaque cycle : une PVC disparue n’est plus exportée. Un timeout ou une erreur d’E/S conserve l’échantillon précédent et incrémente le compteur d’erreurs.

| Métrique | Sens |
|---|---|
| `kubelet_volume_stats_used_bytes` | Octets alloués du répertoire (`st_blocks * 512`), hardlinks dédupliqués par inode |
| `kubelet_volume_stats_capacity_bytes` | Demande `spec.resources.requests.storage` de la PVC. Sans demande, taille totale du système de fichiers. La série est absente si les deux manquent, pour ne pas publier une capacité 0 |
| `kubelet_volume_stats_available_bytes` | `min(capacity - used, espace libre statfs)`, borné à 0. Sans capacité connue, espace libre statfs |
| `kubelet_volume_stats_inodes` | Inodes du système de fichiers entier (`statfs`), pas un quota de PVC |
| `kubelet_volume_stats_inodes_free` | Inodes libres du système de fichiers entier (`statfs`), pas un quota de PVC |
| `kubelet_volume_stats_inodes_used` | Entrées comptées dans le répertoire de la PVC |

`inodes` et `inodes_free` sont identiques pour toutes les PVC d’un même disque : `statfs` décrit le système de fichiers qui porte le répertoire, pas la PVC. Seul `inodes_used` est compté dans le répertoire.
| `local_path_exporter_scan_duration_seconds` | Durée du dernier cycle |
| `local_path_exporter_scan_errors_total` | Erreurs de scan depuis le démarrage |
| `local_path_exporter_scan_last_success_timestamp_seconds` | Unix time du dernier cycle qui a mesuré au moins une PVC, ou qui n’avait rien à mesurer |

Le parcours ne suit pas les liens symboliques et ne descend pas sur un autre `st_dev`. Priorité basse : `nice` 19 et classe d’E/S idle, au mieux.

Port d’écoute : **9100** (`/metrics`, `/healthz`, `/readyz`).

## Ne pas fusionner avec le job kubelet

Les noms `kubelet_volume_stats_*` sont ceux du kubelet pour réutiliser les règles existantes. Sur `local-path`, le kubelet publie le disque racine du nœud, pas le répertoire de la PVC. Si les deux cibles sont scrapées, Prometheus garde deux séries distinguées par `job`. Une alerte ou une somme qui ne filtre pas `job` compte les deux.

Le `ServiceMonitor` fixe `job="local-path-exporter"` et `honorLabels: true`. Sans cela, Prometheus renomme `namespace` et `node` en `exported_namespace` et `exported_node`, parce que la cible porte déjà ces labels. Les règles d’exemple filtrent ce job.

## Mapping

Informers sur `persistentvolumes` et `persistentvolumeclaims` (`get`, `list`, `watch`). `NODE_NAME` vient de l’API downward. L’informer PV est indexé par nœud : chaque cycle ne lit que les PV de ce nœud. Le watch reste sur tout le cluster, parce que le placement (annotation ou affinité) n’est pas un field selector. Un PV est retenu via `local.path.provisioner/selected-node`, sinon `nodeAffinity` (`kubernetes.io/hostname`, `metadata.name`, ou une clé locale-path dont l’unique valeur est le nom du nœud). Le chemin lu est `spec.local.path`, sinon `spec.hostPath.path`, et doit rester sous `STORAGE_PATH`. L’identité vient de `claimRef`. Le nom de répertoire n’est parsé qu’en secours, en coupant `_` par la droite (`pvc-<id>_<namespace>_<pvc>`, l’id peut contenir `_`) : `persistentvolume` vaut alors ce préfixe `pvc-<id>`.

Les quotas projet XFS ne sont pas lus (hors périmètre). Il n’y a pas d’option pour les activer : `local-path` ne pose pas de quota, donc l’usage peut dépasser la demande.

## Variables d’environnement

| Variable | Défaut | Rôle |
|---|---|---|
| `NODE_NAME` | requis | Nom du nœud (`spec.nodeName`) |
| `STORAGE_PATH` | `/var/lib/rancher/k3s/storage` | Répertoire monté en lecture seule |
| `METRICS_ADDR` | `:9100` | Adresse d’écoute |
| `SCAN_INTERVAL` | `5m` | Période entre deux cycles |
| `SCAN_TIMEOUT` | `2m` | Délai max d’un parcours de PVC |
| `SCAN_PARALLELISM` | `2` | Parcours simultanés |

## Déploiement

Manifests d’exemple, base d’une PR GitOps séparée. Ils ne sont pas appliqués par la CI. Aligner le namespace et les labels avec les sélecteurs du Prometheus Operator. `DAC_READ_SEARCH` est requis pour `stat` des fichiers des autres UID : un namespace en Pod Security `restricted` refusera le pod. L’image est distroless, non-root (65532), rootfs en lecture seule.

- `deploy/daemonset.yaml`
- `deploy/serviceaccount.yaml`
- `deploy/clusterrole.yaml`
- `deploy/clusterrolebinding.yaml`
- `deploy/service.yaml`
- `deploy/servicemonitor.yaml`
- `deploy/prometheusrule.yaml`

Image publiée sur `ghcr.io/louclaw/local-path-exporter` uniquement pour un push sur `main` (`:main`, `:sha-<commit>`) et les tags `v*` (`vX.Y.Z`, `X.Y.Z`, `X.Y`, `X`, plus le sha), via `docker/metadata-action`. linux/amd64 et linux/arm64.

Le DaemonSet d’exemple épingle `v0.1.0` avec `imagePullPolicy: IfNotPresent`. Ne pas suivre le tag mouvant `:main`. Au déploiement, remplacer ce tag par le `vX.Y.Z` ou le digest réellement publié.

## Build

```bash
go test ./...
go build -o local-path-exporter ./cmd/local-path-exporter
```
