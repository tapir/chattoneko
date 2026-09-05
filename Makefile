.PHONY: web sqlc build run dev tidy docker mobile mobile-apk \
	mobile-avd mobile-emulator mobile-emulator-wait mobile-emulator-kill \
	mobile-emulator-ensure mobile-install mobile-run mobile-reset

ANDROID_SDK ?= $(HOME)/Android/Sdk
AVD ?= chattoneko
DEVICE ?= emulator-5554
EMULATOR_FLAGS ?= -no-snapshot -gpu swiftshader_indirect -memory 4096
ANDROID_AVD_HOME ?= $(HOME)/.android/avd
SYS_IMG ?= system-images;android-36;google_apis;x86_64
AVD_DEVICE ?= pixel_8

web:
	cd web && npm ci && npm run build

sqlc:
	sqlc generate

build: web sqlc
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o chattoneko .
	@if command -v upx >/dev/null 2>&1; then \
		if upx -t chattoneko >/dev/null 2>&1; then \
			echo "chattoneko already packed, skipping compression"; \
		else \
			upx --best --lzma chattoneko; \
		fi; \
	else \
		echo "upx not found, skipping compression"; \
	fi

run: build
	./chattoneko

dev:
	go run . -db chatto.db

tidy:
	go mod tidy

docker:
	docker build -t chattoneko .

mobile:
	cd mobile && npm ci && npm run sync

mobile-apk: mobile
	cd mobile/android && ./gradlew assembleDebug

# Create the AVD if it doesn't exist; skip otherwise.
mobile-avd:
	@if env ANDROID_AVD_HOME=$(ANDROID_AVD_HOME) $(ANDROID_SDK)/emulator/emulator -list-avds 2>/dev/null | grep -qx '$(AVD)'; then \
		 echo "AVD '$(AVD)' already exists"; \
	 else \
		 echo "creating AVD '$(AVD)' (image: $(SYS_IMG))"; \
		 mkdir -p $(ANDROID_AVD_HOME) && \
		 printf 'no\n' | env ANDROID_SDK_ROOT=$(ANDROID_SDK) ANDROID_AVD_HOME=$(ANDROID_AVD_HOME) \
			 $(ANDROID_SDK)/cmdline-tools/latest/bin/avdmanager create avd \
			 -n '$(AVD)' -k '$(SYS_IMG)' --device '$(AVD_DEVICE)' --force; \
	 fi
	@conf="$(ANDROID_AVD_HOME)/$(AVD).avd/config.ini"; \
	 if [ -f "$$conf" ] && ! grep -q '^hw\.keyboard=yes' "$$conf"; then \
		 sed -i 's/^hw\.keyboard=.*/hw.keyboard=yes/' "$$conf" || echo "hw.keyboard=yes" >> "$$conf"; \
		 echo "enabled hardware keyboard input"; \
	 fi

# Start the emulator in the background (log: /tmp/emulator.log).
mobile-emulator: mobile-avd
	@if adb -s $(DEVICE) get-state >/dev/null 2>&1; then \
		 echo "emulator already running ($(DEVICE))"; \
	 else \
		 nohup env ANDROID_AVD_HOME=$(ANDROID_AVD_HOME) $(ANDROID_SDK)/emulator/emulator -avd $(AVD) $(EMULATOR_FLAGS) > /tmp/emulator.log 2>&1 < /dev/null & \
		 echo "emulator starting (avd: $(AVD)) — wait with: make mobile-emulator-wait"; \
	 fi

# Block until the emulator reports a completed boot.
mobile-emulator-wait:
	@until [ "$$(adb -s $(DEVICE) shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" = "1" ]; do sleep 2; done
	@echo "emulator booted ($(DEVICE))"

# Kill the emulator (timeout-guarded adb, then a precise process match so
# we never touch e.g. the /tmp/chattoneko dev server).
mobile-emulator-kill:
	@timeout 5 adb -s $(DEVICE) emu kill >/dev/null 2>&1 || \
	  pkill -f "emulator.*$(AVD)" 2>/dev/null || true
	@echo "emulator stopped"

# Ensure the emulator is reachable; start one if missing.
mobile-emulator-ensure:
	@adb -s $(DEVICE) get-state >/dev/null 2>&1 || $(MAKE) mobile-emulator mobile-emulator-wait

# Build the APK and install it on the emulator. If `install -r` fails with a
# signature mismatch (e.g. after a keystore change), uninstall and reinstall;
# note this wipes the app's local storage (server URL + API key).
mobile-install: mobile-apk mobile-emulator-ensure
	@adb -s $(DEVICE) install -r mobile/android/app/build/outputs/apk/debug/app-debug.apk || \
	  (echo "install -r failed — uninstalling old build and retrying" && \
	   adb -s $(DEVICE) uninstall com.chattoneko.app && \
	   adb -s $(DEVICE) install mobile/android/app/build/outputs/apk/debug/app-debug.apk)

# Build, install and launch the app.
mobile-run: mobile-install
	adb -s $(DEVICE) shell am start -n com.chattoneko.app/.MainActivity

# Wipe the app's local data (server URL + API key + WebView storage) so the
# next launch is from-scratch: back to the "Connect to a server" screen.
mobile-reset: mobile-emulator-ensure
	adb -s $(DEVICE) shell am force-stop com.chattoneko.app
	adb -s $(DEVICE) shell pm clear com.chattoneko.app

