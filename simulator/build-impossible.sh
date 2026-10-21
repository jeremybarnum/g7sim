#!/bin/sh
# Builds ImpossiBLE's simulator library as a dylib to inject into an app at
# launch, so the app needs no change to talk to g7sim from the iOS Simulator,
# and a second one for a watch app on the watchOS Simulator.
#
#   simulator/build-impossible.sh [out-dir]     (default: simulator/build)
set -eu

# Library 3.1.0, the protocol impossible.go speaks.
repo=https://github.com/mickeyl/ImpossiBLE
commit=58fe38d

here=$(cd "$(dirname "$0")" && pwd)
out=${1:-$here/build}
src=$out/ImpossiBLE
mkdir -p "$out"
if [ ! -d "$src/.git" ]; then
	git clone --quiet "$repo" "$src"
fi
git -C "$src" fetch --quiet origin
git -C "$src" checkout --quiet --force "$commit"
# Fixes proposed upstream, where ImpossiBLE behaves unlike CoreBluetooth in
# ways Loop's device drivers trip on: state callbacks off the central's
# queue (mickeyl/ImpossiBLE#2, #3), a scan filter written as a 128-bit UUID
# missing a 16-bit advert (#4), subscriptions and services surviving a
# disconnect (#5), retrievePeripheralsWithIdentifiers: forgetting
# peripherals on relaunch (#6), and the owner of a connect made inside
# didDisconnect forgotten (0007), and a pending connect reading as
# disconnected, so an app's cancel of it was skipped (0008), a released
# stand-in peripheral aborting the app in CBPeripheral's dealloc (0009), and
# a connection event reaching one central only, without the peripheral's
# name (0010), a released central's connects left pending (0011), and the
# reader's autorelease pool never drained, keeping released centrals (0012);
# and Bluetooth switched off in the app by the provider (0013).
for patch in "$here"/impossible-patches/*.patch; do
	git -C "$src" apply "$patch"
done

build() { # sdk target library
	xcrun -sdk "$1" clang -dynamiclib -fobjc-arc -fmodules \
		-target "$2" \
		-I "$src/Sources/ImpossiBLE/include" -I "$src/Sources/ImpossiBLE" \
		-framework Foundation -framework CoreBluetooth \
		-install_name @rpath/libImpossiBLE.dylib \
		"$src"/Sources/ImpossiBLE/*.m -o "$out/$3"
	echo "$out/$3"
}
build iphonesimulator arm64-apple-ios15.0-simulator libImpossiBLE.dylib
# The same sources serve a watch app unchanged.
build watchsimulator arm64-apple-watchos10.0-simulator libImpossiBLE-watch.dylib
