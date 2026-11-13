#!/bin/sh
# Assembles the Linux release assets of one architecture from the tree that make install-systemd
# install-openrc staged in RELEASE_DIR/root-ARCH: the bare executables, a self-contained tar.gz and a
# Debian/Ubuntu .deb, all written to RELEASE_DIR.
# Usage: packaging/release.sh VERSION RELEASE_DIR ARCH, where ARCH is amd64 or arm64 as Go and Debian name it.
set -eu

if [ $# -ne 3 ]; then
	echo "usage: $0 VERSION RELEASE_DIR ARCH" >&2
	exit 2
fi

version=$1
release=$2
arch=$3
root=$release/root-$arch

# The bare executables and the archive keep the x86_64 name earlier releases used.
case $arch in
amd64) label=x86_64 ;;
arm64) label=arm64 ;;
*)
	echo "unsupported architecture: $arch" >&2
	exit 2
	;;
esac

# Debian versions must start with a digit, and ~ sorts a pre-release such as v1.0.0-rc1 before v1.0.0.
debversion=$(printf '%s' "${version#v}" | tr '-' '~')
case $debversion in
[0-9]*) ;;
*)
	echo "VERSION $version does not map to a Debian version, which must start with a digit" >&2
	exit 2
	;;
esac

command -v dpkg-deb >/dev/null || {
	echo "dpkg-deb is required to build the .deb package" >&2
	exit 2
}

# The archives carry the commit time and root ownership, not the build's, so a rebuild is identical.
epoch=$(git log -1 --format=%ct 2>/dev/null || date +%s)

install -m 0755 "$root/usr/bin/fsledger" "$release/fsledger-linux-$label"
install -m 0755 "$root/usr/sbin/fsledgerd" "$release/fsledgerd-linux-$label"

name=fsledger-$version-linux-$label
tree=$release/$name
mkdir -p "$tree/man" "$tree/systemd" "$tree/openrc"
cp -p "$root/usr/bin/fsledger" "$root/usr/sbin/fsledgerd" "$tree/"
cp -p "$root"/usr/share/man/man*/* "$tree/man/"
cp -pR "$root/etc/fsledger" "$tree/examples"
cp -p "$root/usr/lib/systemd/system/fsledger.service" "$tree/systemd/"
cp -p "$root/etc/init.d/fsledger" "$tree/openrc/fsledger.initd"
cp -p "$root/etc/conf.d/fsledger" "$tree/openrc/fsledger.confd"
cp -p LICENSE README.md README.es.md "$tree/"
tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" -C "$release" -cf - "$name" |
	gzip -9n >"$release/$name.tar.gz"
rm -rf "$tree"

# The .deb installs what make install-systemd does, with the unit in the vendor directory. The example
# configuration becomes conffiles, so an upgrade never overwrites a reviewed /etc/fsledger.
deb=$release/deb-$arch
mkdir -p "$deb/etc" "$deb/usr/share/doc/fsledger" "$deb/DEBIAN"
cp -pR "$root/usr" "$deb/"
cp -pR "$root/etc/fsledger" "$deb/etc/"
gzip -9n "$deb"/usr/share/man/man*/*
install -m 0644 LICENSE "$deb/usr/share/doc/fsledger/copyright"
gzip -9n <README.md >"$deb/usr/share/doc/fsledger/README.md.gz"
(cd "$deb" && find etc -type f | sort | sed 's|^|/|') >"$deb/DEBIAN/conffiles"
size=$(du -sk --exclude=DEBIAN "$deb" | cut -f1)
sed -e "s|@VERSION@|$debversion|" -e "s|@ARCH@|$arch|" -e "s|@SIZE@|$size|" packaging/debian/control \
	>"$deb/DEBIAN/control"
install -m 0755 packaging/debian/postinst packaging/debian/prerm packaging/debian/postrm "$deb/DEBIAN/"
SOURCE_DATE_EPOCH=$epoch dpkg-deb --root-owner-group -Zxz --build "$deb" \
	"$release/fsledger_${debversion}_$arch.deb"
rm -rf "$deb"
