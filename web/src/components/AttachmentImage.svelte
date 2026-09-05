<script>
  // <img> for an image attachment. Three states, by where the picture is:
  //  - staged (att.previewUrl): only the local file exists; show it.
  //  - just uploaded (app.previewFor(id)): the local copy is still on screen
  //    from the pending bubble, so keep painting THAT until the server copy
  //    is fully decoded, then flip. A fresh <img> on the server URL would
  //    paint progressively over a slow link — a picture already on screen
  //    visibly re-drawing itself.
  //  - persisted: the server URL.
  import { api } from '../lib/api.js';
  import { app } from '../lib/state.svelte.js';

  let { att, ...rest } = $props();

  const server = api.attachmentUrl(att.id);
  let src = $state(att.previewUrl || app.previewFor(att.id) || server);

  $effect(() => {
    const blob = app.previewFor(att.id);
    if (att.previewUrl || !blob || src === server) return;
    const im = new Image();
    im.src = server;
    im.decode().then(() => {
      // Same bytes are now in the browser cache, decoded: this is one paint.
      src = server;
      URL.revokeObjectURL(blob);
      app.forgetPreview(att.id);
    }, () => {
      // ponytail: stay on the local copy if the server one won't decode; a
      // reload shows the server's error state, no need to race it here.
    });
  });
</script>

<img {src} alt={att.filename} {...rest} />
