import { useCallback, useRef, useState } from "react";
import * as ReactDOM from "react-dom";
import type { Root } from "react-dom/client";
import { GrafanaTheme2, PluginExtensionPanelContext } from "@grafana/data";
import { config } from "@grafana/runtime";
import { Icon, IconButton, ThemeContext, useStyles2 } from "@grafana/ui";
import { css } from "@emotion/css";
import { ChatInterface } from "./components/features/ChatInterface/ChatInterface";
import {
  normalizeResponseLanguage,
  type ResponseLanguage,
} from "./services/landingText";

/**
 * Chat docked to the edge, with no mask: the dashboard stays visible AND
 * usable.
 *
 * Grafana does have an extension sidebar, but it is restricted to a hardcoded
 * list of plugin ids in its own source (PERMITTED_EXTENSION_SIDEBAR_PLUGINS)
 * -- unreachable for a third-party plugin, and the call is ignored silently.
 * And `openModal` only ever renders a centered modal with a mask: the
 * dashboard shows through, but zooming or changing the time range is gone.
 *
 * Hence mounting straight into document.body. That works here precisely
 * because ChatInterface uses no react-router hooks (see the comment at the top
 * of ChatInterface.tsx): it needs no context from Grafana's React tree, only
 * the theme.
 */

const CONTAINER_ID = "agent-ai-side-panel";

// Width is a lasting preference: a markdown table is unreadable at 480px, and
// nobody wants to drag it wider on every open.
const WIDTH_STORAGE_KEY = "agent_ai_side_panel_width";
// Grafana 13 already uses the right edge for its dashboard edit pane, so which
// side to dock on is a preference, not a constant: on the left, the chat sits
// beside that pane instead of covering it.
const SIDE_STORAGE_KEY = "agent_ai_side_panel_side";
type PanelSide = "left" | "right";
const DEFAULT_WIDTH = 480;
const MIN_WIDTH = 320;

// Bounded to the window width minus a margin: a panel covering the whole
// screen defeats the point, since whatever it was opened over is hidden.
const maxWidth = () => Math.max(MIN_WIDTH, window.innerWidth - 200);
const clampWidth = (value: number) =>
  Math.min(Math.max(value, MIN_WIDTH), maxWidth());

function storedWidth(): number {
  try {
    const raw = window.localStorage.getItem(WIDTH_STORAGE_KEY);
    return raw ? clampWidth(Number(raw)) : DEFAULT_WIDTH;
  } catch {
    // Storage unavailable (private browsing, blocked cookies): the default
    // width stays perfectly usable.
    return DEFAULT_WIDTH;
  }
}

function storedSide(): PanelSide {
  try {
    return window.localStorage.getItem(SIDE_STORAGE_KEY) === "left"
      ? "left"
      : "right";
  } catch {
    return "right";
  }
}

let root: Root | undefined;
let container: HTMLElement | undefined;

type CreateRoot = (container: Element | DocumentFragment) => Root;

/**
 * React 18 (Grafana 12) still exposed `createRoot` on the `react-dom` entry
 * itself; React 19 (Grafana 13) removed it and left it only in
 * `react-dom/client`.
 *
 * Bundling `react-dom/client` is not a way out: the published entry is a
 * two-line shim that re-exports `react-dom`'s own `createRoot`, which is
 * exactly what React 19 dropped -- hence "createRoot is not a function" on
 * Grafana 13, with no build-time warning of any kind.
 *
 * Grafana 13 does share the real module (its `sharedDependencies.ts` maps
 * `react-dom/client` into the SystemJS import map), but Grafana 12 does not,
 * and an `import` of it compiles to a hard AMD dependency that would fail the
 * whole plugin there. So the module is resolved at runtime instead: ask
 * `react-dom` first, and fall back to the shared module only when it has no
 * `createRoot` of its own.
 */
let createRootPromise: Promise<CreateRoot> | undefined;

function getCreateRoot(): Promise<CreateRoot> {
  if (!createRootPromise) {
    const fromReactDom = (ReactDOM as unknown as { createRoot?: CreateRoot })
      .createRoot;
    if (typeof fromReactDom === "function") {
      createRootPromise = Promise.resolve(fromReactDom);
    } else {
      // `window.System` is SystemJS, which Grafana itself uses to load
      // plugins -- so by the time this code runs it is necessarily there.
      // Guarded all the same: a future loader change would otherwise surface
      // as an unreadable "cannot read property import of undefined".
      const system = (
        window as unknown as {
          System?: { import(id: string): Promise<unknown> };
        }
      ).System;
      createRootPromise = system
        ? system.import("react-dom/client").then((module) => {
            const createRoot = (module as { createRoot?: CreateRoot })
              .createRoot;
            if (typeof createRoot !== "function") {
              throw new Error("react-dom/client exposes no createRoot");
            }
            return createRoot;
          })
        : Promise.reject(
            new Error("no react-dom.createRoot and no SystemJS to load it"),
          );
    }
  }
  return createRootPromise;
}


interface SidePanelProps {
  /** Start collapsed: the permanent tab shows, and the chat only expands on
   *  click. */
  startCollapsed?: boolean;
  /** Absent when opened from the command palette: the full chat, with no
   *  pre-filled question, usable while navigating. */
  panelContext?: Readonly<PluginExtensionPanelContext>;
  responseLanguage: ResponseLanguage;
  onClose: () => void;
}

function SidePanel({
  panelContext,
  responseLanguage,
  startCollapsed,
  onClose,
}: SidePanelProps) {
  const styles = useStyles2(getStyles);
  const [width, setWidth] = useState(storedWidth);
  const [collapsed, setCollapsed] = useState(Boolean(startCollapsed));
  const [side, setSide] = useState<PanelSide>(storedSide);

  const flipSide = useCallback(() => {
    setSide((current) => {
      const next: PanelSide = current === "right" ? "left" : "right";
      try {
        window.localStorage.setItem(SIDE_STORAGE_KEY, next);
      } catch {
        // Without persistence the choice holds for this session only.
      }
      return next;
    });
  }, []);
  const draggingRef = useRef(false);

  const onPointerDown = useCallback(
    (event: React.PointerEvent<HTMLDivElement>) => {
      draggingRef.current = true;
      // Pointer capture keeps the events even if the cursor crosses an
      // iframe or leaves the window mid-drag.
      event.currentTarget.setPointerCapture(event.pointerId);
    },
    [],
  );

  const onPointerMove = useCallback(
    (event: React.PointerEvent<HTMLDivElement>) => {
      if (draggingRef.current) {
        // The drag direction depends on which side we are docked to, so
        // `side` must be a dependency -- otherwise flipping sides leaves an
        // inverted calculation behind.
        setWidth(
          clampWidth(
            side === "right" ? window.innerWidth - event.clientX : event.clientX
          )
        );
      }
    },
    [side]
  );

  const onPointerUp = useCallback(
    (event: React.PointerEvent<HTMLDivElement>) => {
      draggingRef.current = false;
      event.currentTarget.releasePointerCapture(event.pointerId);
      try {
        window.localStorage.setItem(WIDTH_STORAGE_KEY, String(width));
      } catch {
        // Without persistence the resize holds for this session only.
      }
    },
    [width],
  );

  // Both elements are ALWAYS rendered, at the same place in the tree: only
  // their style changes. Rendering ChatInterface under a different parent
  // depending on the state made React unmount and remount it, which re-ran its
  // mount effect -- and therefore started a fresh analysis.
  return (
    <>
      <button
        type="button"
        className={collapsed ? styles.collapsedBar : styles.hidden}
        style={collapsed ? { [side]: 0 } : undefined}
        onClick={() => setCollapsed(false)}
        aria-label="Expand Agent AI"
        aria-expanded={!collapsed}
      >
        <Icon name={side === "right" ? "angle-left" : "angle-right"} />
        <Icon name="ai-sparkle" />
      </button>
      <aside
        className={styles.panel}
        style={{
          width,
          [side]: 0,
          display: collapsed ? "none" : "flex",
        }}
        aria-label="Agent AI"
      >
        <div
          className={styles.resizeHandle}
        style={side === "right" ? { left: -3 } : { right: -3 }}
          role="separator"
          aria-orientation="vertical"
          aria-label="Resize panel"
          onPointerDown={onPointerDown}
          onPointerMove={onPointerMove}
          onPointerUp={onPointerUp}
          onDoubleClick={() => setWidth(DEFAULT_WIDTH)}
        />
        <header className={styles.header}>
          <Icon name="ai-sparkle" />
          <span className={styles.title}>Agent AI</span>
          {panelContext && (
            <span className={styles.panelName} title={panelContext.title}>
              {panelContext.title}
            </span>
          )}
          {!panelContext && <span className={styles.panelName} />}
          <IconButton
            name={side === "right" ? "arrow-left" : "arrow-right"}
            aria-label="Move to the other side"
            tooltip="Move to the other side"
            onClick={flipSide}
          />
          <IconButton
            name={side === "right" ? "angle-right" : "angle-left"}
            aria-label="Collapse to the edge"
            tooltip="Collapse -- keeps the conversation"
            onClick={() => setCollapsed(true)}
          />
          <IconButton name="times" aria-label="Close" onClick={onClose} />
        </header>
        <div className={styles.body}>
          <ChatInterface
            panelContext={panelContext}
            onDismiss={onClose}
            allowFollowUp
            responseLanguage={responseLanguage}
          />
        </div>
      </aside>
    </>
  );
}

/** True while the panel is mounted. */
export function isSidePanelOpen(): boolean {
  return root !== undefined;
}

/** Closes the panel and releases the React root. */
export function closeSidePanel() {
  if (root) {
    root.unmount();
    root = undefined;
  }
  if (container) {
    container.remove();
    container = undefined;
  }
}

export function openSidePanel(
  panelContext: Readonly<PluginExtensionPanelContext> | undefined,
  responseLanguage: unknown,
  startCollapsed = false,
) {
  // Resolved before anything is torn down, so a loader failure leaves the
  // panel that is already open untouched instead of closing it for a
  // replacement that never arrives.
  getCreateRoot()
    .then((createRoot) => {
      mountSidePanel(
        createRoot,
        panelContext,
        responseLanguage,
        startCollapsed,
      );
    })
    .catch((error) => {
      console.error("Agent AI: cannot open the side panel", error);
    });
}

function mountSidePanel(
  createRoot: CreateRoot,
  panelContext: Readonly<PluginExtensionPanelContext> | undefined,
  responseLanguage: unknown,
  startCollapsed: boolean,
) {
  // Reopening from another panel replaces the content instead of stacking
  // two panels on top of each other.
  closeSidePanel();

  container = document.createElement("div");
  container.id = CONTAINER_ID;
  document.body.appendChild(container);

  root = createRoot(container);
  root.render(
    // Mounted outside Grafana's tree, so outside its ThemeContext: without
    // this provider @grafana/ui would fall back to the default light theme
    // whatever the user's preference.
    <ThemeContext.Provider value={config.theme2}>
      <SidePanel
        panelContext={panelContext}
        responseLanguage={normalizeResponseLanguage(responseLanguage)}
        startCollapsed={startCollapsed}
        onClose={closeSidePanel}
      />
    </ThemeContext.Provider>,
  );
}

const getStyles = (theme: GrafanaTheme2) => ({
  panel: css({
    position: "fixed",
    top: 0,
    bottom: 0,
    display: "flex",
    flexDirection: "column",
    background: theme.colors.background.primary,
    // Border on both sides: the panel docks left or right, and a single
    // border would leave a bare edge in one of the two cases.
    borderLeft: `1px solid ${theme.colors.border.weak}`,
    borderRight: `1px solid ${theme.colors.border.weak}`,
    boxShadow: theme.shadows.z3,
    // Below modals: a Grafana dialog opened afterwards must stay on top, or
    // it would be unreachable.
    zIndex: theme.zIndex.navbarFixed,
  }),
  // Thin strip on the edge: the hit area overflows the visible line a little,
  // otherwise it is fiddly to grab with a mouse.
  resizeHandle: css({
    position: "absolute",
    top: 0,
    bottom: 0,
    width: 7,
    cursor: "col-resize",
    zIndex: 1,
    "&:hover": {
      background: theme.colors.primary.border,
    },
  }),
  // Collapsed tab: wide enough to aim at, thin enough to hide nothing that
  // matters. Shares the panel's positioning and z-index.
  collapsedBar: css({
    position: "fixed",
    // Vertically centered rather than full height: a full-height strip hides
    // a whole column of dashboard for nothing.
    top: "50%",
    transform: "translateY(-50%)",
    width: 28,
    height: 72,
    borderRadius: theme.shape.radius.default,
    display: "flex",
    flexDirection: "column",
    alignItems: "center",
    justifyContent: "center",
    gap: theme.spacing(0.5),
    padding: 0,
    border: `1px solid ${theme.colors.border.weak}`,
    background: theme.colors.background.secondary,
    color: theme.colors.text.secondary,
    cursor: "pointer",
    zIndex: theme.zIndex.navbarFixed,
    "&:hover": {
      background: theme.colors.action.hover,
      color: theme.colors.text.primary,
    },
  }),
  // The chat stays in the DOM while collapsed: hidden, never unmounted, or an
  // in-flight stream would be cancelled and the exchange lost.
  hidden: css({
    display: "none",
  }),
  header: css({
    display: "flex",
    alignItems: "center",
    gap: theme.spacing(1),
    padding: theme.spacing(1, 1, 1, 2),
    borderBottom: `1px solid ${theme.colors.border.weak}`,
  }),
  title: css({
    fontWeight: theme.typography.fontWeightMedium,
  }),
  panelName: css({
    flex: 1,
    minWidth: 0,
    overflow: "hidden",
    textOverflow: "ellipsis",
    whiteSpace: "nowrap",
    color: theme.colors.text.secondary,
    fontSize: theme.typography.bodySmall.fontSize,
  }),
  body: css({
    flex: 1,
    minHeight: 0,
    overflow: "auto",
  }),
});
