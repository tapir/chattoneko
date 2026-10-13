<script>
  // <img> for a stored image attachment: the server URL, nothing else. A row
  // still on its way up never reaches here — the message shows the loading box
  // for it (MessageItem) — so there is no local copy to paint and no swap to
  // choreograph. The staged lightbox keeps its own previewUrl handling.
  import { api } from '../lib/api.js';

  let { att, ...rest } = $props();
</script>

<img src={api.attachmentUrl(att.id)} alt={att.filename} {...rest} />
