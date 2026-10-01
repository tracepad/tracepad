# Shared by the tag checks (spec 020 #28); sourced, not run.
#
# A tag names a version in one spelling and only one: three numbers without
# leading zeros, and a pre-release of `-alpha.N`, `-beta.N` or `-rc.N`. A tag
# is not taken back from the module proxy or from a registry, and "close
# enough" (`v0.01.0`, `v0.1.0-rc1`) is a second tag for a version that already
# has one.

# canonical_semver <version>: succeeds for X.Y.Z and X.Y.Z-(alpha|beta|rc).N.
canonical_semver() {
    local n='(0|[1-9][0-9]*)'
    printf '%s\n' "$1" | grep -Eq "^$n\\.$n\\.$n(-(alpha|beta|rc)\\.$n)?$"
}
