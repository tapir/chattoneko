# Mobile Architecture

The mobile app is an Android client for Chattoneko. It is **not** a separate product: it is a Capacitor wrapper around the exact same Svelte SPA the server embeds (`web/`), pointed at a user-configured Chattoneko server instead of being served by one. The web frontend is the single source of truth — the app adds no UI of its own.

```
mobile/
  capacitor.config.json    app identity + WebView server settings
  package.json             scripts: icons, sync, apk, adb:install
  scripts/
    icons.mjs              launcher icons from canonical SVGs (rsvg-convert)
    sync.mjs               builds ../web, copies dist → www/, cap sync android
  icons/                   canonical SVG sources (background, foreground, combined)
  www/                     generated web bundle (gitignored, rebuilt by every sync)
  android/                 committed Capacitor Android project
    app/src/main/java/com/chattoneko/app/MainActivity.java
    app/src/main/AndroidManifest.xml
    app/src/main/res/      adaptive + non-adaptive launcher icons, splash, file_paths
    capacitor.settings.gradle, app/capacitor.build.gradle   generated plugin wiring
```

## How it runs

- **Capacitor 8** (`@capacitor/android`, `@capacitor/core`) hosts the SPA in a WebView. `capacitor.config.json`: appId `com.chattoneko.app`, appName `ChattoNeko`, `webDir: www`, `server.androidScheme: http` with `cleartext: true` — so the app can talk to plain-HTTP servers on a LAN (a typical self-hosted setup) — and `android.adjustMarginsForEdgeToEdge: "disable"`, which leaves edge-to-edge inset handling to the SPA's own `.p-safe` CSS.
- The WebView serves the bundled SPA from its own origin (`http://localhost` on Android) and talks to the user's server **cross-origin**. The server cooperates: its CORS middleware whitelists exactly the Capacitor origins (`http://localhost`, `https://localhost`, `capacitor://localhost`) and answers preflights before auth. Credentials are deliberately OFF — the app authenticates with Bearer tokens, never cookies.
- `MainActivity` is an empty `com.getcapacitor.BridgeActivity` subclass: there is no custom Java/Kotlin code. Native capability comes entirely from npm plugins — `@capacitor/app` (back button, `exitApp`), `@capacitor/camera` (camera + gallery), `@capacitor/status-bar` (bar style/color follow the theme) and `@capawesome/capacitor-file-picker` (documents). `cap sync` generates their Gradle wiring (`capacitor.settings.gradle`, `app/capacitor.build.gradle`); each is reached from the SPA through a dynamic `import()` guarded by `isNative()`, so the web bundle never evaluates them.
- The only Android permission the app declares is `INTERNET`, and none of the plugins add more (the camera and pickers go through system intents). A `FileProvider` (`res/xml/file_paths.xml`: external + cache paths) is registered for serving downloaded attachment files.

## Native detection & server configuration

The SPA detects the WebView at runtime via `window.Capacitor.isNativePlatform()` (Capacitor injects it — no npm dependency needed for detection). Native mode changes these things, all in `web/src/lib/server.js` + the app store:

1. **Server address.** `LoginScreen` is one screen with two phases: with no stored server URL it opens in the **address** phase (enter the address → normalize: default `http://` scheme, trim trailing path junk → probe unauthenticated `GET <server>/api/meta` → persist). If the probed server reports auth enabled, the address locks and the credential fields appear below it; if it reports auth disabled, boot continues straight away. The web build renders the same screen without the address field. A deliberate "change server" (sidebar / server-down screen) reopens the address phase without clearing anything until confirmed.
2. **Base URL.** Every REST call and EventSource URL is prefixed with the stored server (`localStorage['chattoneko-server-url']`) instead of same-origin `/api`; `credentials` is `omit`.
3. **Auth without cookies.** Login posts the credentials to `POST /api/auth/login` and stores ONLY the returned JWT (`localStorage['chattoneko-token']`, 90-day TTL). The password is never persisted. REST calls send `Authorization: Bearer <token>`; EventSource streams and attachment `<img>` loads (which can't set headers) append `?token=<jwt>` — the server accepts the query form on GET requests. An expired or rejected token (401) drops the app back to the login screen. With auth disabled on the server, the app just works with no token.
4. **Android back button.** `@capacitor/app`'s `backButton` listener overrides Capacitor's default handling, so `App.svelte` implements the whole fallback chain: close the change-server screen, then the topmost entry of the LIFO overlay registry (`lib/overlays.svelte.js` — every sheet, dialog and popover registers its close callback), then hash-history back, then `exitApp()`.
5. **Status bar.** `@capacitor/status-bar` matches the bar icon style and background color to the current theme.

Everything else — the chat UI, streaming/resume semantics, drafts, attachment staging, sidebar reconciliation — is the identical web implementation (see `docs/frontend.md`), which is built for this dual mode.

## Touch-specific UI

The SPA renders these mobile variants from the same components (viewport/`pointer` queries and `isNative()`, no separate codebase):

| Desktop | Mobile |
| --- | --- |
| File input for attachments | `AttachmentSheet` bottom drawer → camera / photo gallery / file picker (`lib/native-attachments.js`) |
| Popover `Select` for model + reasoning effort | `ModelPickerSheet` bottom drawer (segmented effort control, large tappable model rows) |
| `ConfirmModal` centered `<dialog>` | `ConfirmSheet` bottom action sheet — any dismissal reports cancel |
| Resizable `PanelSheet` side panels | fullscreen sheets, no resize handle |
| Header refresh button | pull-to-refresh on the sidebar list |
| Inline resizable sidebar | fullscreen left `Sheet` |
| — | `.p-safe` padding for status-bar / gesture insets |

The drawers are `vaul-svelte` behind the vendored `ui/drawer` component.

## Build pipeline

The Android project is committed, but the web bundle is always regenerated (`www/` is gitignored):

1. `scripts/icons.mjs` — renders all launcher icon PNGs from the canonical SVGs with `rsvg-convert`: adaptive-icon layers (background + foreground) at 108dp per density bucket (mdpi 108 → xxxhdpi 432px), non-adaptive glyphs (`ic_launcher`, `ic_launcher_round`) at launcher sizes (48 → 192px).
2. `scripts/sync.mjs` — `npm run build` in `web/`, replaces `mobile/www/` with the fresh `web/dist`, then `npx cap sync android` (copies the web assets into the Android project and regenerates plugin wiring).
3. `cd mobile/android && ./gradlew assembleDebug` → APK at `app/build/outputs/apk/debug/app-debug.apk`.

Gradle/Android versions: Android Gradle Plugin 8.13, Java 21 source/target compatibility, `minSdk 24`, `compileSdk`/`targetSdk 36`.

npm scripts (in `mobile/`): `icons`, `sync` (icons + sync), `apk` (sync + assembleDebug), `adb:install`.

Repo-root Makefile targets wrap emulator development (`ANDROID_SDK`, `AVD=chattoneko`, `DEVICE=emulator-5554`, `SYS_IMG=system-images;android-36;google_apis;x86_64`, `AVD_DEVICE=pixel_8`, all overridable):

| Target | Does |
| --- | --- |
| `mobile` | `npm ci` + `npm run sync` in `mobile/` |
| `mobile-apk` | `mobile` + `./gradlew assembleDebug` |
| `mobile-avd` | create the AVD if missing (and enable hardware keyboard input) |
| `mobile-emulator` | start the AVD headless in the background (log: `/tmp/emulator.log`) |
| `mobile-emulator-wait` | block until `sys.boot_completed` |
| `mobile-emulator-kill` | `adb emu kill`, falling back to a precise process match |
| `mobile-emulator-ensure` | start + wait only if the device is unreachable |
| `mobile-install` | build the APK and install it, uninstalling first on a signature mismatch |
| `mobile-run` | `mobile-install` + launch the activity |
| `mobile-reset` | `pm clear` — wipes server URL, JWT and WebView storage, so the next launch starts at the address phase |

## What lives where

| Concern | Location |
| --- | --- |
| All UI + app logic | `web/` (shared with the server-embedded SPA) |
| Server URL, JWT | WebView `localStorage` (per-app sandbox) |
| Chats, messages, files | the user's Chattoneko server — nothing chat-related is stored on the device |
| Native shell, icons, manifest | `mobile/android/` |
| Launcher icon sources | `mobile/icons/*.svg` |
| Server-side support for the app | CORS origin allowlist + Bearer/`?token=` JWT auth (see `docs/backend.md`) |
