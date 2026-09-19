<script>
	// Root input plus a show/hide toggle for secret values (passwords, API
	// keys). `class` stays layout-only and lands on the inner input; `label`
	// only names the value in the toggle's aria-label.
	import EyeIcon from "@lucide/svelte/icons/eye";
	import EyeOffIcon from "@lucide/svelte/icons/eye-off";
	import { cn } from "$lib/utils.js";
	import Root from "./input.svelte";

	let {
		ref = $bindable(null),
		value = $bindable(),
		label = "password",
		class: className,
		"data-slot": dataSlot = "input-password",
		...restProps
	} = $props();

	let revealed = $state(false);
</script>

<div class="relative" data-slot={dataSlot}>
	<Root
		bind:ref
		bind:value
		type={revealed ? "text" : "password"}
		class={cn("pr-9", className)}
		{...restProps}
	/>
	<button
		type="button"
		class="absolute inset-y-0 right-0 flex w-9 items-center justify-center text-muted-foreground transition-colors hover:text-foreground"
		aria-label={revealed ? `Hide ${label}` : `Show ${label}`}
		onclick={() => (revealed = !revealed)}
	>
		{#if revealed}
			<EyeOffIcon class="size-4" strokeWidth={1.75} aria-hidden="true" />
		{:else}
			<EyeIcon class="size-4" strokeWidth={1.75} aria-hidden="true" />
		{/if}
	</button>
</div>
