export const brand = {
  navy: '#050B2F',
  navySoft: '#11194A',
  // Muted dusty-rose sampled from the fox logo's own fur tone -- everywhere
  // this used to be a bright, saturated red it now matches the mascot
  // instead. The corner glow in ChatInterface.styles.ts (radial-gradient at
  // top-left/bottom-right) is intentionally NOT derived from this constant
  // and keeps its original rgba(184, 32, 25, ...) values untouched.
  red: '#C4534F',
  redSoft: '#D07571',
  // Vivid, fully-opaque blue for the send button -- navy read as near-black
  // against the dark theme background, making it look faded/invisible
  // rather than a distinct "send" affordance.
  sendBlue: '#2B6ADB',
  surface: '#FFFFFF',
  border: 'rgba(196, 83, 79, 0.22)',
};
