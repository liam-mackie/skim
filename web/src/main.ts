import { mount } from 'svelte';
import './app.css';
import App from './App.svelte';
import { installExternalLinks } from './lib/external-links';

void installExternalLinks();

export default mount(App, { target: document.getElementById('app')! });
