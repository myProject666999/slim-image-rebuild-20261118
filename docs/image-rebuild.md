# Image rebuild (slimmed image reconstruction)

`pkg/docker/imagerebuild` rewrites a container image so that it contains only
the files that are visible in the image's final filesystem view. It is a
library: callers hand it a `v1.Image` (go-containerregistry) and get back a
rebuilt image plus a set of counters.

```go
res, err := imagerebuild.Rebuild(src, imagerebuild.Options{
    Mode:         imagerebuild.ModePreserveLayers, // default
    KeepHistory:  false,                           // default
    ScratchDir:   "/var/tmp",                      // optional
})
if err != nil { ... }
defer res.Close() // removes the temporary layer spools

// res.Image is a v1.Image; write it to a daemon / registry / tarball as usual.
```

## Chosen approach: preserve the original layer sequence

Two organizations were considered:

| | Single layer | Preserve layers (chosen) |
|---|---|---|
| Output size for one image | smallest | slightly larger (a few small layers instead of one) |
| Layer reuse across images | none: every slimmed image carries its own full copy of the base data | kept: untouched layers keep their digests and stay shared |
| Aggregate storage for many slimmed variants | grows with every variant | grows only by the changed layers |
| Implementation | simple | needs to attribute each surviving file to the layer it came from |

The single-layer layout is the simplest and produces the smallest artifact for
a standalone image, but it throws away layer reuse. When a base image is
slimmed into several variants (different filter sets, different entrypoints,
per-service images), each variant would embed its own complete copy of the base
filesystem, so the registry and the local store end up holding more bytes than
before the slimming. That defeats the purpose of slimming a fleet of images.

Preserving the layers keeps sharing intact: any layer whose entries all survive
untouched is copied byte-for-byte and keeps its digest (see
`Result.ReusedLayers`), so the slimmed image continues to share that blob with
its base image and with sibling slimmed images. `ModeSingleLayer` is still
available for callers that only ever ship one artifact and want the smallest
possible one.

The cost of the chosen approach is the "which layer owns the file" problem: a
path may be written by several layers and the visible version comes from the
topmost one, while a whiteout or an opaque marker in a higher layer may remove
it entirely. The library resolves this by building the final view first, then
writing each surviving entry back into the layer it originally came from,
dropping the entries that lost and the layers that end up empty.

## Guarantees

### Final view

The rebuilt image's final view (the result of applying the layers in order)
matches the source exactly for every path in it:

* file content, size, mode (including setuid/setgid/sticky), uid/gid, uname/gname,
  mtime (including sub-second precision) and extended attributes,
* symlink targets,
* directory entries, including their permissions and timestamps,
* hard links (see below).

Entries hidden by a whiteout (`.wh.<name>`) or by an opaque directory marker
(`.wh..wh..opq` / `.wh..opq`) never reappear: the markers themselves are not
copied, and the shadowed lower-layer entries are not written back. The whiteout
and aufs metadata namespaces (`.wh..wh.*`, e.g. the hard link store
`.wh..wh.plnk`) are never part of the visible filesystem and are dropped.

A directory written by a higher layer merges with the lower layer's directory
at the same path (Docker semantics), while a non-directory replaces whatever was
there, including a subtree.

### Hard links

* If both names of a hard link pair survive, the pair stays a hard link: one
  name carries the data, the other is a `TypeLink` entry pointing at it. The
  chain shape of the source is respected where possible (a link to a link stays
  a link to a link).
* If only one name survives, the other name disappears and the survivor keeps
  the content. When a later layer replaced the link's target with other
  content, the link keeps the *original* data, as in the source.
* If all names are gone, nothing is written.
* Links pointing into the aufs hard link store are materialized with the store's
  content, and the store itself is not copied.

### Configuration

The rebuilt config is copied from the source and only `rootfs.diff_ids` and
`history` are recomputed. This preserves, verbatim:

* `Entrypoint` and `Cmd`, including the exec-vs-shell distinction and the
  argument order,
* environment variables, including their order and duplicate keys
  (`PATH=/a` and `PATH=/b` are both kept, in order),
* `WorkingDir`, `User`, `ExposedPorts`, `Volumes`, `Labels`, `StopSignal`,
  `Shell`, `ArgsEscaped`, `Healthcheck` (including `CMD` vs `CMD-SHELL` and the
  zero values),
* `Architecture`, `OS`, `OSVersion`, `OSFeatures`, `Variant`, `Author`,
  `Container`, `DockerVersion`, `Created`,
* the manifest media type, the config media type and manifest annotations.

The library does not normalize between exec and shell forms, so signal delivery
and argument handling are unchanged.

### Determinism

The same input image rebuilt twice produces the same manifest digest:

* layer file order is deterministic: the source order is kept in
  `ModePreserveLayers` (and an untouched layer is copied byte-for-byte), and
  paths are sorted in `ModeSingleLayer`, with hard link targets written before
  the links,
* timestamps come from the source headers; nothing uses the wall clock,
* the gzip level is a constant (`gzip.BestSpeed`), and go-containerregistry
  writes the gzip header without mtime,
* PAX records are sorted by the Go tar writer, and the library always requests
  PAX on rewritten headers so long names, sub-second times and xattrs survive,
* by default the history is dropped (see below), so two images with the same
  filesystem view but different build histories still get the same digest.

## History

`Options.KeepHistory` selects the behavior:

* **`false` (default):** the output history is empty. This is the recommended
  mode for reproducible digests: history entries carry build timestamps, host
  names and commands that no longer describe the rewritten layers, and they are
  the main source of digest instability between otherwise identical images.
* **`true`:** `ModePreserveLayers` copies the history entries of the layers
  that were retained (and all `EmptyLayer` entries, in order). The entries of
  dropped layers are removed, so the number of non-empty entries still matches
  the number of layers. `ModeSingleLayer` collapses the layers, so only the
  `EmptyLayer` entries can be carried over; per-layer entries are dropped.

Keeping the history makes the digest depend on the source history, including
its timestamps. That is acceptable when reconstructing an image that must keep
its provenance, but it means rebuilding the same filesystem with a different
build time yields a different digest.

## Compatibility with the existing slim pipeline

The repository currently builds slimmed images in two ways:

* the internal engine (`pkg/imagebuilder/internalbuilder`) flattens everything
  into one tar (`files.tar`) and appends it as a single layer (`ADD files.tar /`),
* the Docker/BuildKit engine generates a Dockerfile whose `COPY`/`ADD` also
  adds a single data layer on top of the base image.

Both produce images with a **different number of layers and different digests
than this library**. Anything keyed on the digest of a slimmed image (image
pinning, admission policies, `slim.report.json` consumers, CI caches keyed by
digest, signed images) will see a different digest after switching engines.
Nothing in this repository stores or compares slimmed-image digests across
builds, but downstream users may.

To keep both worlds available:

* `ModeSingleLayer` reproduces the storage shape of the current engines (one
  data layer, deterministic bytes) without depending on a Docker daemon, so it
  can be adopted incrementally. It is *not* byte-identical to the current
  `files.tar` layer (different compression and ordering), so digests still
  change once.
* `ModePreserveLayers` is the default in this package but is **not** the
  default behavior of the existing build engines; it must be selected
  explicitly by the caller. The existing engines are untouched, so current
  pipelines keep their behavior and digests.
* There is no compatibility flag inside `pkg/docker/imagerebuild` itself, since
  the package is new and has no previous digest contract. If the build
  engines are later switched to use it, the engine selection flag
  (`--image-build-engine`) remains the user-facing switch, and a new engine
  value should be introduced rather than changing the output of `internal` in
  place.

## Limits

* Only single-platform images are handled. An OCI image index must be resolved
  to a platform image by the caller.
* Only `linux` images are supported (no Windows layer semantics).
* A link whose target cannot be resolved inside the image (a broken hard link
  already present in the source) is copied unchanged.
