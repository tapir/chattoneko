<script>
  // Shared icon-only button: consistent size, hover and focus styling for
  // every toolbar/row action in the app.
  let {
    icon: Icon, // lucide component
    label, // aria-label + tooltip
    onclick = null,
    type = 'button',
    size = 'md', // 'sm' (rows/hover actions) | 'md' (toolbars) | 'lg' (header)
    danger = false,
    disabled = false,
    class: cls = '',
    iconClass = '',
  } = $props();

  const sizes = {
    sm: ['size-6', 'size-3.5'],
    md: ['size-7', 'size-3.5'],
    lg: ['size-9', 'size-[18px]'],
  };
  let [btnSize, iconSize] = $derived(sizes[size] ?? sizes.md);
</script>

<button
  {type}
  class={[
    'flex shrink-0 items-center justify-center rounded-md transition-colors disabled:opacity-40',
    danger
      ? 'text-muted-foreground hover:bg-destructive/10 hover:text-destructive'
      : 'text-muted-foreground hover:bg-accent hover:text-foreground',
    btnSize,
    cls
  ]}
  title={label}
  aria-label={label}
  {onclick}
  {disabled}
>
  <Icon class={iconClass || iconSize} strokeWidth={1.75} aria-hidden="true" />
</button>
