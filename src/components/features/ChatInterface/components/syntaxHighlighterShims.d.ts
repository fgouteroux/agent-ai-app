declare module 'react-syntax-highlighter/dist/esm/prism-light' {
  import { SyntaxHighlighterProps } from 'react-syntax-highlighter';
  import { ComponentType } from 'react';
  const PrismLight: ComponentType<SyntaxHighlighterProps> & { registerLanguage: (name: string, language: unknown) => void };
  export default PrismLight;
}

declare module 'react-syntax-highlighter/dist/esm/languages/prism/*' {
  const language: unknown;
  export default language;
}
