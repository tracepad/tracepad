# Shared by the tag checks (spec 020 #28); sourced, not run.
#
# A tag names a version in one spelling and only one: three numbers without
# leading zeros, and a pre-release of `-alpha.N`, `-beta.N` or `-rc.N`. A tag
# is not taken back from the module proxy or from a registry, and "close
# enough" (`v0.01.0`, `v0.1.0-rc1`) is a second tag for a version that already
# has one. The server's tags are held to exactly that by scripts/release-tag.sh
# (spec 020 #27), and the packages' versions are asked of it, so that the
# grammar is defined once.

# canonical_semver <version>: succeeds for X.Y.Z and X.Y.Z-(alpha|beta|rc).N.
canonical_semver() {
    "$(dirname "${BASH_SOURCE[0]}")/release-tag.sh" "v$1" >/dev/null 2>&1
}
