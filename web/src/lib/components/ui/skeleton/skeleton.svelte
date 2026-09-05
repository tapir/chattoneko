<script>
	import { cn } from "$lib/utils.js";
	let {
		ref = $bindable(null),
		class: className,
		...restProps
	} = $props();
</script>

<div
	bind:this={ref}
	data-slot="skeleton"
	class={cn("rounded-md bg-muted animate-pulse", className)}
	{...restProps}
></div>