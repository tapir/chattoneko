<script>
  // The one loading spinner for the whole app. `light` = for placement on
  // primary-colored surfaces (e.g. the sign-in button).
  let { class: cls = 'size-4', light = false, label = 'Loading' } = $props();
</script>

<span
  role="status"
  aria-label={label}
  class={[
    'inline-block animate-spin rounded-full border-2',
    light
      ? 'border-primary-foreground/30 border-t-primary-foreground'
      : 'border-muted-foreground/30 border-t-foreground',
    cls
  ]}
></span>
