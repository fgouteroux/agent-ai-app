import React, { useEffect, useRef, useState } from 'react';
import { css } from '@emotion/css';
import { GrafanaTheme2 } from '@grafana/data';
import { locationService } from '@grafana/runtime';
import { Alert, Button, ConfirmModal, Field, Icon, IconButton, Input, Slider, TextArea, useStyles2 } from '@grafana/ui';
import { fetchAgentsConfig, saveAgentsConfig, SettingsConflictError, type AgentsConfig } from '../api/client';
import { colorForAgent, MAX_CUSTOM_AGENTS } from '../agentColors';
import { PLUGIN_BASE_URL } from '../constants';

// Mirrors pkg/plugin/agents.go's maxAgentContextChars -- this text is
// injected into every request's system prompt for that agent, on top of
// the skill pack/persona/guardrails. Realistic free-tier LLM endpoints
// (e.g. Groq's free tier tops out around 6-12k tokens per minute total)
// can be pushed into rate-limit/request-too-large errors by a system
// prompt that's too large before the user has typed a single message.
const MAX_CONTEXT_CHARS = 4000;
// Reject an uploaded file outright above this -- avoids reading a huge
// file into memory just to truncate almost all of it away.
const MAX_UPLOAD_BYTES = 51200; // 50 KB

const agentId = (n: number) => `agent-${n}`;
const defaultLabel = (n: number) => `Agent ${n}`;

// Mirrors pkg/plugin/compaction.go's genericMaxContextTokens/
// maxCustomAgentContextTokens -- a custom agent's context window can be
// raised (in 5k steps) from a sensible starting point up to the hard cap.
const MIN_CONTEXT_TOKENS = 100000;
const MAX_CONTEXT_TOKENS = 120000;
const DEFAULT_CONTEXT_TOKENS = 100000;
const CONTEXT_TOKENS_STEP = 5000;

interface AgentSlot {
  n: number;
  id: string;
}

export function AgentsPage() {
  const styles = useStyles2(getStyles);
  const [activeCount, setActiveCount] = useState(0);
  const [savedLabels, setSavedLabels] = useState<Record<string, string>>({});
  const [savedContexts, setSavedContexts] = useState<Record<string, string>>({});
  const [savedTemperatures, setSavedTemperatures] = useState<Record<string, number>>({});
  const [savedContextTokens, setSavedContextTokens] = useState<Record<string, number>>({});
  // Optimistic-concurrency token for the shared plugin settings blob -- see
  // saveSettingsWithVersionCheck in api/client.ts. Bumped on every successful
  // save; a mismatch on the next save means another admin (or another open
  // tab) saved a change in between, and persist() below surfaces that as a
  // distinct, actionable error instead of silently overwriting it.
  const [version, setVersion] = useState(0);
  const [isLoading, setIsLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [pendingDelete, setPendingDelete] = useState<AgentSlot | null>(null);
  const fileInputRefs = useRef<Record<string, HTMLInputElement | null>>({});

  // Editing state: a card is either locked (showing saved* values) or, for
  // at most one card at a time, unlocked with its own draft buffer -- edits
  // only take effect on the backend once explicitly saved and confirmed.
  const [editingId, setEditingId] = useState<string | null>(null);
  const [draftLabel, setDraftLabel] = useState('');
  const [draftContext, setDraftContext] = useState('');
  const [draftTemperature, setDraftTemperature] = useState<number | undefined>(undefined);
  const [draftContextTokens, setDraftContextTokens] = useState(DEFAULT_CONTEXT_TOKENS);
  const [resetNonce, setResetNonce] = useState(0);
  const [contextTokensResetNonce, setContextTokensResetNonce] = useState(0);
  const [pendingSaveEdit, setPendingSaveEdit] = useState(false);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const config = await fetchAgentsConfig();
        if (cancelled) {
          return;
        }
        setSavedLabels(config.labels);
        setSavedContexts(config.contexts);
        setSavedTemperatures(config.temperatures);
        setSavedContextTokens(config.contextTokens);
        setActiveCount(config.activeCount);
        setVersion(config.version);
      } catch (e: any) {
        if (!cancelled) {
          setError(e.message || 'Failed to load agents');
        }
      } finally {
        if (!cancelled) {
          setIsLoading(false);
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const slots: AgentSlot[] = Array.from({ length: activeCount }, (_, i) => ({ n: i + 1, id: agentId(i + 1) }));
  const isEditing = editingId !== null;

  const labelFor = (id: string, n: number) =>
    id === editingId ? draftLabel : savedLabels[id]?.trim() || defaultLabel(n);
  const contextFor = (id: string) => (id === editingId ? draftContext : savedContexts[id] ?? '');
  const temperatureFor = (id: string) => (id === editingId ? draftTemperature : savedTemperatures[id]);
  const contextTokensFor = (id: string) => (id === editingId ? draftContextTokens : savedContextTokens[id]);

  async function persist(config: Omit<AgentsConfig, 'version'>) {
    setBusy(true);
    setError(null);
    try {
      const newVersion = await saveAgentsConfig({ ...config, version });
      setSavedLabels(config.labels);
      setSavedContexts(config.contexts);
      setSavedTemperatures(config.temperatures);
      setSavedContextTokens(config.contextTokens);
      setActiveCount(config.activeCount);
      setVersion(newVersion);
      return true;
    } catch (e: any) {
      if (e instanceof SettingsConflictError) {
        setError(e.message);
      } else {
        setError(e.message || 'Failed to save. Nothing was changed.');
      }
      return false;
    } finally {
      setBusy(false);
    }
  }

  const startEditing = (id: string, n: number) => {
    setEditingId(id);
    setDraftLabel(savedLabels[id] ?? defaultLabel(n));
    setDraftContext(savedContexts[id] ?? '');
    setDraftTemperature(savedTemperatures[id]);
    setDraftContextTokens(savedContextTokens[id] ?? DEFAULT_CONTEXT_TOKENS);
    setResetNonce(0);
    setContextTokensResetNonce(0);
    setError(null);
  };

  const cancelEditing = () => {
    setEditingId(null);
  };

  const onChangeDraftLabel = (value: string) => setDraftLabel(value);
  const onChangeDraftContext = (value: string) => setDraftContext(value.slice(0, MAX_CONTEXT_CHARS));
  const onChangeDraftTemperature = (value: number) => setDraftTemperature(value);
  const onResetDraftTemperature = () => {
    setDraftTemperature(undefined);
    setResetNonce((n) => n + 1);
  };
  const onResetDraftContextTokens = () => {
    setDraftContextTokens(DEFAULT_CONTEXT_TOKENS);
    setContextTokensResetNonce((n) => n + 1);
  };

  const onDownload = (id: string, n: number) => {
    const blob = new Blob([contextFor(id)], { type: 'text/markdown' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `${labelFor(id, n).toLowerCase().replace(/\s+/g, '-')}.md`;
    a.click();
    URL.revokeObjectURL(url);
  };

  const onUploadClick = (id: string) => {
    fileInputRefs.current[id]?.click();
  };

  const onFileSelected = (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    event.target.value = '';
    if (!file) {
      return;
    }
    if (file.size > MAX_UPLOAD_BYTES) {
      setError(`"${file.name}" is too large (${Math.round(file.size / 1024)} KB). Max upload size is ${MAX_UPLOAD_BYTES / 1024} KB.`);
      return;
    }
    const reader = new FileReader();
    reader.onload = () => {
      const text = String(reader.result || '');
      if (text.length > MAX_CONTEXT_CHARS) {
        setError(`"${file.name}" was truncated to ${MAX_CONTEXT_CHARS} characters (the per-agent context limit).`);
      }
      onChangeDraftContext(text);
    };
    reader.readAsText(file);
  };

  const onAddAgent = async () => {
    if (activeCount >= MAX_CUSTOM_AGENTS || busy || isEditing) {
      return;
    }
    const newCount = activeCount + 1;
    const ok = await persist({
      labels: savedLabels,
      contexts: savedContexts,
      temperatures: savedTemperatures,
      contextTokens: savedContextTokens,
      activeCount: newCount,
    });
    if (ok) {
      startEditing(agentId(newCount), newCount);
    }
  };

  const requestDeleteAgent = (slot: AgentSlot) => {
    if (busy || isEditing) {
      return;
    }
    setPendingDelete(slot);
  };

  const confirmDeleteAgent = async () => {
    if (!pendingDelete) {
      return;
    }
    const { n: deletedN } = pendingDelete;

    // Slots are positional (agent-N's color/ID never changes based on its
    // name), so deleting the middle of the list shifts everything after it
    // down by one instead of leaving a gap -- e.g. deleting agent-2 out of
    // 3 makes the old agent-3's label/context/temperature become agent-2's.
    const shift = <T,>(data: Record<string, T>): Record<string, T> => {
      const next = { ...data };
      for (let k = deletedN; k < activeCount; k++) {
        const fromId = agentId(k + 1);
        const toId = agentId(k);
        if (fromId in next) {
          next[toId] = next[fromId];
        } else {
          delete next[toId];
        }
      }
      delete next[agentId(activeCount)];
      return next;
    };

    const ok = await persist({
      labels: shift(savedLabels),
      contexts: shift(savedContexts),
      temperatures: shift(savedTemperatures),
      contextTokens: shift(savedContextTokens),
      activeCount: activeCount - 1,
    });
    if (ok) {
      setPendingDelete(null);
    }
  };

  const requestSaveEdit = () => {
    const candidate = draftLabel.trim();
    if (candidate.toLowerCase() === 'default') {
      setError('"Default" is reserved for the built-in agent -- choose another name.');
      return;
    }
    const clash = slots.find((s) => s.id !== editingId && labelFor(s.id, s.n).trim().toLowerCase() === candidate.toLowerCase());
    if (clash) {
      setError(`Another agent is already named "${candidate}" -- agent names must be unique.`);
      return;
    }
    setError(null);
    setPendingSaveEdit(true);
  };

  const confirmSaveEdit = async () => {
    if (!editingId) {
      return;
    }
    const newLabels = { ...savedLabels, [editingId]: draftLabel };
    const newContexts = { ...savedContexts, [editingId]: draftContext };
    const newTemperatures = { ...savedTemperatures };
    if (draftTemperature === undefined) {
      delete newTemperatures[editingId];
    } else {
      newTemperatures[editingId] = draftTemperature;
    }
    const newContextTokens = { ...savedContextTokens, [editingId]: draftContextTokens };

    const ok = await persist({
      labels: newLabels,
      contexts: newContexts,
      temperatures: newTemperatures,
      contextTokens: newContextTokens,
      activeCount,
    });
    if (ok) {
      setPendingSaveEdit(false);
      setEditingId(null);
    }
  };

  if (isLoading) {
    return <div data-testid="agents-page-loading">Loading...</div>;
  }

  const editingSlot = slots.find((s) => s.id === editingId);

  return (
    <div data-testid="agents-page" className={styles.pageShell}>
      <div className={styles.container}>
        <button
          type="button"
          className={styles.backLink}
          onClick={() => {
            // "Manage agents..." (from an active conversation) passes ?return=
            // with the exact chat+session URL to go back to -- falls back to a
            // fresh chat when opened directly (e.g. from the side nav).
            const returnTo = new URLSearchParams(locationService.getLocation().search).get('return');
            locationService.push(returnTo || `${PLUGIN_BASE_URL}/chat`);
          }}
          data-testid="agents-back-to-chat"
        >
          <Icon name="arrow-left" size="sm" />
          Back to chat
        </button>

        <Alert title="Custom specialist agents" severity="info" className={styles.intro}>
          This plugin ships with no built-in specialization. Give each agent below a name and a focus by writing
          (or uploading) context text -- domain knowledge, terminology, where things live in your Grafana
          instance, how to approach questions in that area. The agent then layers this on top of its normal
          Grafana tool-calling ability. An agent with no context configured is blocked from selection in the
          chat (it falls back to Default instead). Click the pencil to edit an agent, make your changes, then
          Save and confirm. Requires Grafana Admin permission.
        </Alert>

        {error && (
          <Alert title="Notice" severity="warning" onRemove={() => setError(null)}>
            {error}
          </Alert>
        )}

        <div className={styles.grid}>
        {slots.map((slot) => {
          const { id, n } = slot;
          const color = colorForAgent(id);
          const editingThis = id === editingId;
          const label = labelFor(id, n);
          const context = contextFor(id);
          const temp = temperatureFor(id);
          const hasTempOverride = temp !== undefined;
          const contextTokens = contextTokensFor(id);

          return (
            <div key={id} className={styles.card} style={{ borderLeftColor: color }}>
              <div className={styles.cardHeader}>
                <span className={styles.colorDot} style={{ background: color }} />
                {editingThis ? (
                  <Input
                    aria-label={`${id} name`}
                    data-testid={`agent-label-${id}`}
                    value={draftLabel}
                    onChange={(e) => onChangeDraftLabel(e.currentTarget.value)}
                    maxLength={40}
                    className={styles.nameInput}
                    autoFocus
                  />
                ) : (
                  <span className={styles.lockedName} data-testid={`agent-label-${id}`}>
                    {label}
                  </span>
                )}
                {editingThis ? (
                  <>
                    <Button variant="secondary" size="sm" onClick={cancelEditing} disabled={busy}>
                      Cancel
                    </Button>
                    <Button size="sm" onClick={requestSaveEdit} disabled={busy} data-testid={`agent-save-${id}`}>
                      Save
                    </Button>
                  </>
                ) : (
                  <>
                    <IconButton
                      name="pen"
                      tooltip={`Edit ${label}`}
                      disabled={busy || isEditing}
                      onClick={() => startEditing(id, n)}
                    />
                    <IconButton
                      name="trash-alt"
                      tooltip={`Delete ${label}`}
                      variant="destructive"
                      disabled={busy || isEditing}
                      onClick={() => requestDeleteAgent(slot)}
                    />
                  </>
                )}
              </div>

              {editingThis ? (
                <>
                  <Field
                    label="Context"
                    description={`Domain knowledge for this agent. ${context.length} / ${MAX_CONTEXT_CHARS} characters.`}
                  >
                    <TextArea
                      aria-label={`${label} context`}
                      data-testid={`agent-context-${id}`}
                      value={context}
                      onChange={(e) => onChangeDraftContext(e.currentTarget.value)}
                      placeholder="Describe this agent's specialization: domain, terminology, where things live, how to approach questions in this area..."
                      rows={7}
                      maxLength={MAX_CONTEXT_CHARS}
                    />
                  </Field>

                  <div className={styles.actions}>
                    <input
                      type="file"
                      accept=".md,.txt"
                      style={{ display: 'none' }}
                      ref={(el) => { fileInputRefs.current[id] = el; }}
                      onChange={onFileSelected}
                      data-testid={`agent-upload-${id}`}
                    />
                    <Button variant="secondary" size="sm" onClick={() => onUploadClick(id)} icon="upload">
                      Upload .md
                    </Button>
                    <Button variant="secondary" size="sm" onClick={() => onDownload(id, n)} icon="download-alt">
                      Download .md
                    </Button>
                  </div>

                  <Field
                    label="Temperature"
                    description="Controls response randomness. Lower (e.g. 0.2) gives more focused, consistent answers; higher (e.g. 1.2+) gives more varied, creative ones. Leave untouched to use the provider's own default."
                    className={styles.temperatureField}
                  >
                    <div className={`${styles.temperatureRow} ${styles.temperatureSlider}`}>
                      <Slider
                        key={`${id}-${resetNonce}`}
                        min={0}
                        max={2}
                        step={0.1}
                        value={temp ?? 1}
                        onChange={onChangeDraftTemperature}
                        inputId={`agent-temperature-${id}`}
                      />
                      {hasTempOverride && (
                        <Button variant="secondary" fill="text" size="sm" onClick={onResetDraftTemperature}>
                          Reset to default
                        </Button>
                      )}
                    </div>
                  </Field>

                  <Field
                    label="Context window (k tokens)"
                    description={`How much conversation history this agent can hold before older messages get summarized. Up to ${MAX_CONTEXT_TOKENS / 1000}k.`}
                    className={styles.temperatureField}
                  >
                    <div className={`${styles.temperatureRow} ${styles.contextTokensSlider}`}>
                      <Slider
                        key={`${id}-ctx-${contextTokensResetNonce}`}
                        min={MIN_CONTEXT_TOKENS / 1000}
                        max={MAX_CONTEXT_TOKENS / 1000}
                        step={CONTEXT_TOKENS_STEP / 1000}
                        value={draftContextTokens / 1000}
                        onChange={(v) => setDraftContextTokens(v * 1000)}
                        inputId={`agent-context-tokens-${id}`}
                      />
                      {draftContextTokens !== DEFAULT_CONTEXT_TOKENS && (
                        <Button variant="secondary" fill="text" size="sm" onClick={onResetDraftContextTokens}>
                          Reset to default
                        </Button>
                      )}
                    </div>
                  </Field>
                </>
              ) : (
                <>
                  <div className={styles.lockedContext} data-testid={`agent-context-${id}`}>
                    {context ? context : <span className={styles.lockedEmpty}>No context configured yet.</span>}
                  </div>
                  <div className={styles.lockedTemp}>
                    Temperature: {hasTempOverride ? temp!.toFixed(1) : 'default'}
                    {' -- '}
                    Context: {contextTokens ? `${contextTokens / 1000}k tokens` : 'default'}
                  </div>
                </>
              )}
            </div>
          );
        })}

        {activeCount < MAX_CUSTOM_AGENTS && (
          <button
            type="button"
            className={styles.addCard}
            onClick={onAddAgent}
            disabled={busy || isEditing}
            data-testid="agent-add"
          >
            <Icon name="plus-circle" size="xxl" />
            <span>Add agent</span>
            <span className={styles.addCardSub}>
              {activeCount} / {MAX_CUSTOM_AGENTS}
            </span>
          </button>
        )}
        </div>
      </div>

      <ConfirmModal
        isOpen={pendingDelete !== null}
        title="Delete agent"
        body={
          pendingDelete
            ? `Delete "${savedLabels[pendingDelete.id] ?? defaultLabel(pendingDelete.n)}"? Its context, name, and temperature setting will be lost permanently.`
            : ''
        }
        confirmText="Delete"
        confirmButtonVariant="destructive"
        onConfirm={confirmDeleteAgent}
        onDismiss={() => setPendingDelete(null)}
      />

      <ConfirmModal
        isOpen={pendingSaveEdit}
        title="Save agent"
        body={
          editingSlot
            ? `Save changes to "${draftLabel || defaultLabel(editingSlot.n)}"? ${
                draftContext
                  ? 'It will use this context for future conversations.'
                  : 'It still has no context configured, so it will stay unavailable in the chat picker until you add some.'
              }`
            : ''
        }
        confirmText="Save"
        onConfirm={confirmSaveEdit}
        onDismiss={() => setPendingSaveEdit(false)}
      />
    </div>
  );
}

function getStyles(theme: GrafanaTheme2) {
  return {
    pageShell: css({
      minHeight: 'calc(100vh - 96px)',
      width: '100%',
      background: `
        radial-gradient(circle at top left, rgba(184, 32, 25, 0.16), transparent 34%),
        radial-gradient(circle at bottom right, rgba(5, 11, 47, 0.14), transparent 42%),
        linear-gradient(180deg, rgba(184, 32, 25, 0.05), transparent 44%)
      `,
    }),
    container: css({
      padding: theme.spacing(2),
      display: 'flex',
      flexDirection: 'column',
      gap: theme.spacing(2),
      maxWidth: '1100px',
    }),
    intro: css({
      marginBottom: 0,
    }),
    backLink: css({
      display: 'flex',
      alignItems: 'center',
      gap: theme.spacing(0.5),
      alignSelf: 'flex-start',
      background: 'transparent',
      border: 'none',
      padding: 0,
      color: theme.colors.text.secondary,
      cursor: 'pointer',
      fontSize: theme.typography.bodySmall.fontSize,
      '&:hover': {
        color: theme.colors.text.primary,
      },
    }),
    grid: css({
      display: 'grid',
      gridTemplateColumns: 'repeat(auto-fill, minmax(360px, 1fr))',
      gap: theme.spacing(2),
      alignItems: 'start',
    }),
    card: css({
      background: theme.colors.background.secondary,
      border: `1px solid ${theme.colors.border.weak}`,
      borderLeft: '4px solid',
      borderRadius: theme.shape.radius.default,
      padding: theme.spacing(2),
      display: 'flex',
      flexDirection: 'column',
      gap: theme.spacing(1.5),
    }),
    cardHeader: css({
      display: 'flex',
      alignItems: 'center',
      gap: theme.spacing(1),
    }),
    colorDot: css({
      width: '10px',
      height: '10px',
      borderRadius: '50%',
      flexShrink: 0,
    }),
    nameInput: css({
      flex: 1,
    }),
    lockedName: css({
      flex: 1,
      fontWeight: 500,
      fontSize: theme.typography.h6.fontSize,
    }),
    lockedContext: css({
      fontSize: theme.typography.bodySmall.fontSize,
      color: theme.colors.text.secondary,
      whiteSpace: 'pre-wrap',
      maxHeight: '140px',
      overflowY: 'auto',
    }),
    lockedEmpty: css({
      fontStyle: 'italic',
    }),
    lockedTemp: css({
      fontSize: theme.typography.bodySmall.fontSize,
      color: theme.colors.text.secondary,
    }),
    actions: css({
      display: 'flex',
      gap: theme.spacing(1),
    }),
    temperatureField: css({
      marginBottom: 0,
    }),
    temperatureRow: css({
      display: 'flex',
      alignItems: 'center',
      gap: theme.spacing(1),
    }),
    // Two sliders with very different jobs (randomness vs. memory size)
    // shouldn't look identical -- distinct accent colors targeting
    // rc-slider's own stable class names (Grafana's Slider has no color prop).
    // Both sliders read the same way: green at the low/safe end, red at the
    // high/max end -- a static gradient across the full rail (not something
    // that visually changes with the handle), with the filled "track"
    // portion made transparent so the gradient underneath is what shows.
    // Temperature's range (0-2) has a real midpoint default (1, "the
    // provider's own default") -- blue at the cold/deterministic low end,
    // green at that neutral default in the middle, red at the hot/creative
    // max, rather than a flat two-stop gradient.
    temperatureSlider: css`
      .rc-slider-rail { background: linear-gradient(to right, #2196F3, #4CAF50, #E53935); }
      .rc-slider-track { background-color: transparent; }
      .rc-slider-handle { background-color: #ffffff; border: 2px solid rgba(0, 0, 0, 0.3); }
    `,
    contextTokensSlider: css`
      .rc-slider-rail { background: linear-gradient(to right, #4CAF50, #E53935); }
      .rc-slider-track { background-color: transparent; }
      .rc-slider-handle { background-color: #ffffff; border: 2px solid rgba(0, 0, 0, 0.3); }
    `,
    addCard: css({
      display: 'flex',
      flexDirection: 'column',
      alignItems: 'center',
      justifyContent: 'center',
      gap: theme.spacing(1),
      minHeight: '160px',
      background: 'transparent',
      border: `2px dashed ${theme.colors.border.medium}`,
      borderRadius: theme.shape.radius.default,
      color: theme.colors.text.secondary,
      cursor: 'pointer',
      '&:disabled': {
        opacity: 0.5,
        cursor: 'not-allowed',
      },
      '&:hover:not(:disabled)': {
        borderColor: theme.colors.primary.border,
        color: theme.colors.text.primary,
        background: theme.colors.action.hover,
      },
    }),
    addCardSub: css({
      fontSize: theme.typography.bodySmall.fontSize,
      color: theme.colors.text.secondary,
    }),
  };
}
