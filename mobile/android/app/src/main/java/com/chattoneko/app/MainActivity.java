package com.chattoneko.app;

import android.net.http.SslError;
import android.os.Bundle;
import android.webkit.SslErrorHandler;
import android.webkit.WebView;
import com.getcapacitor.BridgeActivity;
import com.getcapacitor.BridgeWebViewClient;

// The WebView cancels every request whose certificate fails validation, so a
// self-signed server reports as plain "cannot reach server". There is no
// setting that ignores bad certificates — this callback is the only lever — so
// the login screen's address phase offers an opt-out switch (server.js) and
// the handler reads it back from the page when an error arrives, rather than
// mirroring it into Java over a bridge that would race the first page load.
// Anything but an explicit true cancels, so the default stays "verify".
// ponytail: Chromium caches an allow decision per host+certificate for the
// process lifetime, so switching off mid-session does not re-verify a host it
// already let through — an app restart does. Nothing here can clear that.
public class MainActivity extends BridgeActivity {

    private static final String INSECURE_CHECK = "localStorage.getItem('chattoneko-insecure-tls') === '1'";

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        // super.onCreate swallows a WebView-creation failure (no WebView on the
        // device) and returns without building the bridge.
        if (this.bridge == null) return;
        this.bridge.setWebViewClient(new BridgeWebViewClient(this.bridge) {
            @Override
            public void onReceivedSslError(WebView view, SslErrorHandler handler, SslError error) {
                view.evaluateJavascript(INSECURE_CHECK, value -> {
                    if ("true".equals(value)) handler.proceed();
                    else handler.cancel();
                });
            }
        });
    }
}
