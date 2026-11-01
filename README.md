# local-path-exporter

Mesure l’usage disque réel de chaque PVC `local-path` sur k3s. Les volumes vivent dans `/var/lib/rancher/k3s/storage` (`pvc-<id>_<namespace>_<pvc>`) sans plafond : une PVC demandée à 2 Gio peut occuper des dizaines de Gio. `KubePersistentVolumeFillingUp` ne voit que le disque du nœud.

DaemonSet, un pod par nœud. Pas de driver CSI, pas de `hostNetwork`. Le répertoire de stockage est monté en lecture seule. Le cache de parcours vit dans un `emptyDir` : le rootfs reste en lecture seule. Un scan tourne en tâche de fond (défaut 5 min) ; `/metrics` sert le cache.

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
| `local_path_exporter_scan_duration_seconds` | Durée du dernier cycle |
| `local_path_exporter_scan_entries` | Entrées réellement lues pendant le dernier cycle. Un répertoire servi par le cache, ou un volume sauté, compte pour zéro |
| `local_path_exporter_scan_cache_bytes` | Taille estimée du cache de répertoires en mémoire |
| `local_path_exporter_scan_load_current_bytes` | `memory.current` juste après le chargement du cache persisté |
| `local_path_exporter_scan_throttle_pauses_total` | Pauses du scan complet pendant que `memory.current` dépassait le seuil |
| `local_path_exporter_scan_throttle_pause_seconds` | Secondes passées dans ces pauses depuis le démarrage du processus |
| `local_path_exporter_scan_errors_total` | Erreurs de scan depuis le démarrage. Un scan complet reporté au cycle suivant n’en est pas une |
| `local_path_exporter_scan_last_success_timestamp_seconds` | Unix time du dernier cycle qui a mesuré au moins une PVC, ou qui n’avait rien à mesurer |
| `local_path_exporter_cgroup_current_bytes` | `memory.current` du cgroup du processus |
| `local_path_exporter_cgroup_working_set_bytes` | `memory.current` moins `inactive_file`. Le page cache inactif est récupérable ; le slab, lui, reste dans ce working set |
| `local_path_exporter_cgroup_anon_bytes` | Mémoire anonyme imputée au cgroup |
| `local_path_exporter_cgroup_file_bytes` | Page cache imputé au cgroup |
| `local_path_exporter_cgroup_slab_reclaimable_bytes` | Slab noyau récupérable (dentries, inodes) |
| `local_path_exporter_cgroup_slab_unreclaimable_bytes` | Slab noyau non récupérable |
| `local_path_exporter_cgroup_kernel_bytes` | Mémoire noyau imputée au cgroup |
| `local_path_exporter_go_heap_bytes` | Heap Go (`HeapAlloc`), distinct des compteurs cgroup |

`inodes` et `inodes_free` sont identiques pour toutes les PVC d’un même disque : `statfs` décrit le système de fichiers qui porte le répertoire, pas la PVC. Seul `inodes_used` est compté dans le répertoire.

Le parcours ne suit pas les liens symboliques et ne descend pas sur un autre `st_dev`. Les entrées d’un répertoire sont lues par lots (`getdents`) et comptées au fil de l’eau : l’arborescence n’est pas accumulée. La déduplication des hardlinks ne retient que les inodes avec `nlink > 1`. Le heap Go reste petit. Le cgroup, lui, est aussi facturé pour le slab (dentries, inodes) et le page cache des métadonnées : `GOMEMLIMIT` ne les couvre pas. Sur un `memory.max` d’au plus 512 Mio, le parcours demande à cgroup v2 (`memory.reclaim`) de rendre du cache quand l’usage dépasse les 5/8 de la limite, jusqu’aux 9/16. À la fin du volume, un second appel redescend vers la moitié. Aller plus bas pendant le parcours relit les tables d’inodes. Priorité basse : `nice` 19 et classe d’E/S idle, au mieux. Entre deux volumes, les pages Go inutilisées sont aussi rendues au système.

Le `mtime` d’un répertoire change à la création, la suppression ou le renommage d’un nom, pas quand un fichier déjà présent est réécrit sur place (vérifié sur ext4). Le cache réutilise alors l’ancienne taille jusqu’au prochain scan complet (`FULL_SCAN_INTERVAL`, 6 h). Il est borné par `SCAN_CACHE_MAX_BYTES` ; au-delà, le volume est oublié et reparcouru. Un hardlink nouveau, dont l’autre nom est encore compté comme fichier simple, déclenche un scan complet du volume pour ne pas compter l’inode deux fois.

Le cache ne retient qu’un enregistrement par répertoire : inode, mtime, taille du répertoire, taille agrégée des fichiers directs, et les noms des sous-répertoires. Il n’y a pas d’entrée par fichier. Un hardlink dans un répertoire dont le mtime a changé relance un scan complet du volume, pour ne pas compter l’inode deux fois. Avec l’ancien plafond de 8 Mio, un volume d’environ 90 000 répertoires était jeté en entier et repassait en scan complet à chaque cycle (immich vers 55 min, nullclaw vers 100 min). Le plafond configuré, s’il vaut au moins 8 Mio, est relevé jusqu’à 32 Mio : les deux gros volumes (environ 1,6 million d’entrées) restent en cache, et le scan complet ne revient qu’au bout de `FULL_SCAN_INTERVAL`. Sur un arbre d’un million d’entrées dont 100 000 sont des répertoires, le cache occupe 12,7 Mio (heap mesuré 13,0 Mio) et le fichier persistant 6,9 Mio. Le même million de fichiers dans huit répertoires occupe 1,2 Kio. 1,6 million d’entrées de ce profil restent sous 32 Mio. Ce heap est de la mémoire anonyme, pas le cache noyau qui remplissait les 128 Mio.

Ce cache est aussi écrit dans `SCAN_CACHE_PATH` (un fichier sur l’`emptyDir`, format `LPEC` version 2, la même structure compacte). L’écriture est atomique : fichier temporaire dans le même répertoire, `fsync`, puis `rename`. Un redémarrage recharge ce fichier et reprend en incrémental. Sans ce fichier, chaque démarrage referait le scan complet qui a tué le pod. Le rootfs reste en lecture seule ; rien n’est écrit sur le disque du nœud.

`memory.high` vaut `max` dans le cgroup que pose le kubelet. Le noyau ne réclame donc pas tant que `memory.current` n’a pas atteint `memory.max`, et une pause en dessous du plafond ne fait pas baisser le slab. La charge qui pousse au plafond est le slab récupérable (dentries et inodes, de l’ordre de 1,3 Mio pour 1 000 fichiers), pas le heap Go : anon reste autour de quelques Mio. L’OOM en production est la course entre cette allocation et la réclamation directe au plafond. Découper un seul volume en morceaux ne suffit pas : le slab s’accumule d’un morceau à l’autre tant que le processus et le cgroup vivent.

Chaque parcours, complet ou incrémental, est borné de deux façons. D’abord un débit plafond, `SCAN_MAX_ENTRIES_PER_SEC` (défaut 4000, `0` le coupe), indépendant de `memory.current` : un incrémental qui passe 132 000 entrées en 0,6 s le fait alors que le cgroup est encore vers 57 Mio, sous le seuil, donc sans aucune pause mémoire. Ensuite, tous les `SCAN_THROTTLE_BATCH` entrées (défaut 2048), si `memory.current` dépasse `SCAN_THROTTLE_HIGH` (défaut 100 Mio), backoff de 20 ms jusqu’à 200 ms, au plus 500 ms par lot. Ces pauses repoussent `SCAN_TIMEOUT`. `memory.reclaim` reste tenté et ignoré s’il renvoie EROFS.

Un million d’entrées froides instancie environ 1,4 Kio de slab par entrée (dentry 192 o + inode ext4 1 160 o), soit environ 1,32 Gio, qui restent imputés au cgroup tant que `memory.reclaim` n’est pas inscriptible. À débit nul ce slab ne tient pas dans 128 Mio. Le débit borne la course avec la réclamation directe ; il ne réduit pas le résidu si le noyau ne réclame pas du tout les dentries.

Le fichier de cache est lu et écrit au fil de l’eau. Après lecture, et après le `fsync` de l’écriture, `posix_fadvise(DONTNEED)` retire ses pages du page cache : elles sont facturées au cgroup et s’ajoutent au heap du cache décodé. Un fichier trop gros, tronqué ou illisible est supprimé. Le processus continue avec un cache vide au lieu de reboucler dessus. `local_path_exporter_scan_load_current_bytes` est `memory.current` juste après ce chargement.

Un cycle ne lance qu’un scan complet. Les autres volumes qui en auraient besoin attendent le cycle suivant, gardent leur échantillon précédent, et n’incrémentent pas `local_path_exporter_scan_errors_total`. Deux gros volumes (immich et nullclaw) ne sont donc plus parcourus l’un derrière l’autre au démarrage.

`local_path_exporter_cgroup_working_set_bytes` vaut `memory.current` moins `inactive_file`. Le slab reste dans ce working set : `ContainerMemoryNearLimit` peut sonner pendant le scan complet même quand une partie du page cache est inactive. Après le scan, le slab ne redescend pas tout seul ; seul le cycle incrémental évite d’en allouer encore.

L’intervalle d’un volume est `SCAN_INTERVAL` multiplié par le nombre de pas de `SCAN_ENTRIES_PER_STEP` entrées suivies (minimum un pas), plafonné par `FULL_SCAN_INTERVAL`. `statfs` reste un seul appel. Les répertoires sont ouverts avec `O_NOATIME` (repli sans ce flag si `EPERM`). `posix_fadvise(DONTNEED)` n’est pas utilisé : sur un million d’entrées il ne baisse ni le slab, ni `memory.current`, ni le working set.

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
| `SCAN_PARALLELISM` | `1` | Parcours simultanés. Un volume à la fois évite d’empiler deux pics |
| `SCAN_ENTRIES_PER_STEP` | `50000` | Nombre d’entrées suivies pour un pas d’intervalle. L’attente est `SCAN_INTERVAL` fois le nombre de pas (minimum 1), plafonnée par `FULL_SCAN_INTERVAL`. `0` désactive l’espacement |
| `FULL_SCAN_INTERVAL` | `6h` | Scan complet, pour voir les fichiers réécrits sans création ni suppression. Entre 6 h et 24 h |
| `SCAN_CACHE_MAX_BYTES` | `8388608` | Plafond du cache des répertoires. `0` ne garde rien. Une valeur entre 8 Mio et 32 Mio est relevée à 32 Mio, pour ne pas jeter un gros volume. Au-delà de 32 Mio, la valeur est le plafond |
| `SCAN_CACHE_PATH` | vide | Fichier du cache sur un volume accessible en écriture. Vide : le cache meurt avec le processus |
| `SCAN_THROTTLE_HIGH` | `104857600` | Seuil de `memory.current` (octets) au-dessus duquel le scan complet attend. `0` désactive |
| `SCAN_THROTTLE_BATCH` | `2048` | Entrées entre deux lectures de `memory.current`. `0` désactive le seuil mémoire |
| `SCAN_MAX_ENTRIES_PER_SEC` | `4000` | Débit max de tout parcours, même sous le seuil mémoire. `0` le coupe |
| `GOMEMLIMIT` | `100MiB` | Plafond souple du heap Go seulement. Il ne couvre ni le slab ni le page cache |

## Déploiement

Manifests d’exemple, base d’une PR GitOps séparée. Ils ne sont pas appliqués par la CI. Aligner le namespace et les labels avec les sélecteurs du Prometheus Operator.

Le pod est en UID 0, `allowPrivilegeEscalation: false`, rootfs en lecture seule, `drop: [ALL]` et seulement `DAC_READ_SEARCH`. Un UID non-root ne reçoit pas cette capability dans l’ensemble effectif quand `no_new_privs` est posé (pas de capability ambiante, et une file capability `setcap` est ignorée). En UID 0, le runtime la met dans l’ensemble effectif, ce qui suffit pour `stat` un répertoire 0700 d’un autre utilisateur. Ce n’est pas `DAC_OVERRIDE` : pas d’écriture. Une file capability sur le binaire est moins fiable (xattr perdue par le `COPY` multi-stage vers distroless, et bloquée par `allowPrivilegeEscalation: false`). Un namespace en Pod Security `restricted` refusera le pod. L’image est distroless.

- `deploy/daemonset.yaml`
- `deploy/serviceaccount.yaml`
- `deploy/clusterrole.yaml`
- `deploy/clusterrolebinding.yaml`
- `deploy/service.yaml`
- `deploy/servicemonitor.yaml`
- `deploy/prometheusrule.yaml`

Image publiée sur `ghcr.io/louclaw/local-path-exporter` uniquement pour un push sur `main` (`:main`, `:sha-<commit>`) et les tags `v*` (`vX.Y.Z`, `X.Y.Z`, `X.Y`, `X`, plus le sha), via `docker/metadata-action`. linux/amd64 et linux/arm64.

Le DaemonSet d’exemple épingle `v0.1.6` avec `imagePullPolicy: IfNotPresent`. La limite mémoire reste 128 Mio. Aucune capability n’est ajoutée. Ne pas suivre le tag mouvant `:main`. Au déploiement, remplacer ce tag par le `vX.Y.Z` ou le digest réellement publié.

Chaque volume écrit une ligne `scan volume` : durée, entrées, `full`, pauses, inodes, octets, `cgroup_current`, et les deltas anon, file, slab, kernel, heap. Une ligne `defer full scan` marque un volume reporté. Après déploiement, sur le nœud chargé : le premier cycle ne doit pas finir en `OOMKilled` ; `local_path_exporter_scan_throttle_pauses_total` augmente pendant le gros volume ; un redémarrage du pod doit logger un cache rechargé et un `entries` très petit sur ce volume. `memory.current` peut rester proche de 128 Mio après le scan complet, parce que le slab n’est pas rendu tant que le cgroup n’est pas au plafond. `memory.reclaim` n’existe qu’en cgroup v2 (noyau 5.19 ou plus récent) et renvoie souvent EROFS dans le pod : les gauges restent lisibles.

## Build

```bash
go test ./...
go build -o local-path-exporter ./cmd/local-path-exporter
```
