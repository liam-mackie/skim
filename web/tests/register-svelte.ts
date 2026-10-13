import { readFileSync } from 'node:fs';
import { registerHooks } from 'node:module';
import { compile, compileModule } from 'svelte/compiler';
import ts from 'typescript';

// Run the real browser-side rune/component code in Node's deterministic test runner.
registerHooks({
  load(url, context, nextLoad) {
    if (url.endsWith('.svelte.ts') || url.endsWith('.svelte.js')) {
      const input = readFileSync(new URL(url), 'utf8');
      const source = url.endsWith('.ts') ? ts.transpileModule(input, {
        compilerOptions: { target: ts.ScriptTarget.ESNext, module: ts.ModuleKind.ESNext, useDefineForClassFields: true },
      }).outputText : input;
      return { format: 'module', shortCircuit: true, source: compileModule(source, { filename: new URL(url).pathname, generate: 'client' }).js.code };
    }
    if (url.endsWith('.svelte')) {
      return { format: 'module', shortCircuit: true, source: compile(readFileSync(new URL(url), 'utf8'), { filename: new URL(url).pathname, generate: 'client' }).js.code };
    }
    return nextLoad(url, context);
  },
});
