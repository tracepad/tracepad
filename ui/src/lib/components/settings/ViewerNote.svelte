<script lang="ts">
	// Why the buttons are gone (spec 028 #14, spec 007 #12): a viewer sees what
	// the project is set to and one line saying why they cannot change it. A
	// control that is simply missing leaves a person wondering whether the
	// feature exists; this says who to ask.

	let { what }: { what: string } = $props();
</script>

<p class="text-subtle mt-3 text-sm">Your role in this project is viewer, so {what}.</p>
