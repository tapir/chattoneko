<script>
	import { Slider as SliderPrimitive } from "bits-ui";
	import { cn } from "$lib/utils.js";

	let {
		ref = $bindable(null),
		value = $bindable(0),
		min = 0,
		max = 100,
		step = 1,
		disabled = false,
		// Accessible name for the thumb — the only part carrying role="slider"
		// (same `label` contract as components/IconButton.svelte).
		label = undefined,
		class: className,
		...restProps
	} = $props();
</script>

<!--
	Single value, single thumb: the app's only slider is the audio scrub bar.
	ponytail: a range slider is type="multiple" here plus one Thumb per entry of
	the root snippet's `thumbItems` — add it when something needs it.

	The visible bar is 6px, which no finger can hit, so `py-2 -my-2` grows the
	interactive box to 22px without moving the layout. `touch-pan-y` (not the
	touch-none bits-ui demos use) keeps the page scrollable from a swipe that
	starts on the bar: a vertical pan cancels the drag, a horizontal one is ours.
-->
<SliderPrimitive.Root
	bind:ref
	bind:value
	type="single"
	{min}
	{max}
	{step}
	{disabled}
	data-slot="slider"
	class={cn(
		"relative -my-2 flex w-full touch-pan-y items-center py-2 select-none data-disabled:opacity-50",
		className
	)}
	{...restProps}
>
	<span
		data-slot="slider-track"
		class="relative h-1.5 w-full grow overflow-hidden rounded-full bg-muted-foreground/25"
	>
		<SliderPrimitive.Range
			data-slot="slider-range"
			class="absolute h-full rounded-full bg-primary"
		/>
	</span>
	<SliderPrimitive.Thumb
		data-slot="slider-thumb"
		index={0}
		aria-label={label}
		class="block size-3.5 shrink-0 cursor-pointer rounded-full bg-primary shadow-xs transition-colors focus-visible:ring-3 focus-visible:ring-ring/50 focus-visible:outline-none disabled:pointer-events-none disabled:opacity-50"
	/>
</SliderPrimitive.Root>
