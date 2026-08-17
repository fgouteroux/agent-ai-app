// Importing PrismLight from the package ROOT ('react-syntax-highlighter')
// still pulls in the full ~250-language Prism bundle as a side effect of
// evaluating that module's index (confirmed empirically -- module.js size
// was unchanged). The dedicated subpath below uses refractor/core, which
// starts with ZERO languages registered -- only what we register below
// ends up in the bundle. Types for these submodule paths come from
// ./syntaxHighlighterShims.d.ts (not shipped by @types/react-syntax-highlighter).
import PrismLight from 'react-syntax-highlighter/dist/esm/prism-light';
import json from 'react-syntax-highlighter/dist/esm/languages/prism/json';
import yaml from 'react-syntax-highlighter/dist/esm/languages/prism/yaml';
import bash from 'react-syntax-highlighter/dist/esm/languages/prism/bash';
import javascript from 'react-syntax-highlighter/dist/esm/languages/prism/javascript';
import typescript from 'react-syntax-highlighter/dist/esm/languages/prism/typescript';
import python from 'react-syntax-highlighter/dist/esm/languages/prism/python';
import sql from 'react-syntax-highlighter/dist/esm/languages/prism/sql';
import markdown from 'react-syntax-highlighter/dist/esm/languages/prism/markdown';
import promql from 'react-syntax-highlighter/dist/esm/languages/prism/promql';
import hcl from 'react-syntax-highlighter/dist/esm/languages/prism/hcl';

// The default `Prism` export from react-syntax-highlighter bundles ~250
// language grammars (the single biggest contributor to this plugin's
// module.js size). PrismLight only registers the languages we actually
// register below -- everything else in the code blocks this app renders
// (JSON tool args/dashboards, YAML, bash, PromQL, a bit of JS/TS/Python/SQL)
// is covered; an unregistered language (e.g. logql, traceql, mermaid) just
// renders unhighlighted plain text instead of colorized -- no crash.
PrismLight.registerLanguage('json', json);
PrismLight.registerLanguage('yaml', yaml);
PrismLight.registerLanguage('bash', bash);
PrismLight.registerLanguage('sh', bash);
PrismLight.registerLanguage('javascript', javascript);
PrismLight.registerLanguage('js', javascript);
PrismLight.registerLanguage('typescript', typescript);
PrismLight.registerLanguage('ts', typescript);
PrismLight.registerLanguage('python', python);
PrismLight.registerLanguage('sql', sql);
PrismLight.registerLanguage('markdown', markdown);
PrismLight.registerLanguage('promql', promql);
PrismLight.registerLanguage('hcl', hcl);
PrismLight.registerLanguage('terraform', hcl);
// LogQL has no dedicated Prism grammar upstream. Its surface syntax (label
// selectors `{label="value"}`, string/regex filters, pipe-chained functions)
// is close enough to PromQL's that reusing that grammar highlights braces,
// strings, and operators reasonably -- not perfect, but real color instead
// of fully unhighlighted plain text (the previous behavior for any
// unregistered language, per PrismLight.registerLanguage's own contract).
PrismLight.registerLanguage('logql', promql);

export { PrismLight as SyntaxHighlighter };
