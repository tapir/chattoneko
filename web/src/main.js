import { mount } from 'svelte';
import './app.css';
import App from './App.svelte';
import { loadMarkdown } from './lib/markdown.js';

const app = mount(App, { target: document.querySelector('#app') });

// Warm the lazily-loaded markdown chunk right after first paint: the chunk
// sits off the critical path, but should be in memory before the first
// assistant message needs it, so that message renders as formatted HTML
// instead of briefly showing the plain-text fallback. Non-blocking: nothing
// awaits this.
loadMarkdown();

export default app;
