import React, { useCallback, useEffect, useState } from 'react';
import { css } from '@emotion/css';
import { AppPluginMeta, GrafanaTheme2, PageLayoutType } from '@grafana/data';
import { PluginPage } from '@grafana/runtime';
import { Alert, Button, IconButton, LoadingPlaceholder, Select, TextArea, useStyles2 } from '@grafana/ui';
import { testIds } from '../components/testIds';

type AppPluginSettings = {
  farsightUrl?: string;
};

interface Props {
  meta: AppPluginMeta<AppPluginSettings>;
}

interface ChatMessage {
  role: 'user' | 'assistant';
  content: string;
}

interface ConversationSummary {
  id: number;
  title: string;
  updated_at: string;
}

function Chat({ meta }: Props) {
  const s = useStyles2(getStyles);
  const farsightUrl = (meta.jsonData?.farsightUrl || '').replace(/\/$/, '');

  const [tenants, setTenants] = useState<string[]>([]);
  const [tenant, setTenant] = useState<string>('');
  const [tenantsError, setTenantsError] = useState<string>('');
  const [conversations, setConversations] = useState<ConversationSummary[]>([]);
  const [conversationId, setConversationId] = useState<number | undefined>(undefined);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState('');
  const [sending, setSending] = useState(false);
  const [loadingHistory, setLoadingHistory] = useState(false);
  const [error, setError] = useState('');

  const loadConversations = useCallback(
    (selectedTenant: string, autoLoadLatest: boolean) => {
      if (!farsightUrl || !selectedTenant) {
        return;
      }
      fetch(`${farsightUrl}/llm/conversations?tenant_id=${encodeURIComponent(selectedTenant)}`)
        .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`${r.status} ${r.statusText}`))))
        .then((data: { conversations: ConversationSummary[] }) => {
          const list = data.conversations || [];
          setConversations(list);
          if (autoLoadLatest && list.length > 0) {
            loadConversation(selectedTenant, list[0].id);
          }
        })
        .catch((e) => setError(String(e)));
      // eslint-disable-next-line react-hooks/exhaustive-deps
    },
    [farsightUrl]
  );

  const loadConversation = (selectedTenant: string, id: number) => {
    setLoadingHistory(true);
    fetch(`${farsightUrl}/llm/conversations/${id}/messages?tenant_id=${encodeURIComponent(selectedTenant)}`)
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`${r.status} ${r.statusText}`))))
      .then((data: { conversation_id: number; messages: ChatMessage[] }) => {
        setConversationId(data.conversation_id);
        setMessages(data.messages || []);
      })
      .catch((e) => setError(String(e)))
      .finally(() => setLoadingHistory(false));
  };

  const startNewConversation = () => {
    setConversationId(undefined);
    setMessages([]);
  };

  const deleteConversation = (id: number) => {
    if (!window.confirm('Delete this conversation? This cannot be undone.')) {
      return;
    }
    fetch(`${farsightUrl}/llm/conversations/${id}/delete?tenant_id=${encodeURIComponent(tenant)}`, {
      method: 'POST',
    })
      .then((r) => {
        if (!r.ok) {
          throw new Error(`${r.status} ${r.statusText}`);
        }
        setConversations((prev) => prev.filter((c) => c.id !== id));
        if (id === conversationId) {
          startNewConversation();
        }
      })
      .catch((e) => setError(String(e)));
  };

  useEffect(() => {
    if (!farsightUrl) {
      return;
    }
    fetch(`${farsightUrl}/llm/my-tenants`)
      .then((r) => (r.ok ? r.json() : Promise.reject(new Error(`${r.status} ${r.statusText}`))))
      .then((data: { tenants: string[] }) => {
        setTenants(data.tenants || []);
        if (data.tenants && data.tenants.length > 0) {
          setTenant(data.tenants[0]);
          loadConversations(data.tenants[0], true);
        }
      })
      .catch((e) => setTenantsError(String(e)));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [farsightUrl]);

  const send = () => {
    if (!input.trim() || !tenant || sending) {
      return;
    }
    const userMessage: ChatMessage = { role: 'user', content: input };
    setMessages((prev) => [...prev, userMessage]);
    setInput('');
    setSending(true);
    setError('');

    fetch(`${farsightUrl}/llm/chat`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ tenant_id: tenant, conversation_id: conversationId, message: userMessage.content }),
    })
      .then((r) => {
        if (!r.ok) {
          return r.text().then((t) => {
            throw new Error(t || `${r.status} ${r.statusText}`);
          });
        }
        return r.json();
      })
      .then((data: { conversation_id: number; reply: string }) => {
        setConversationId(data.conversation_id);
        setMessages((prev) => [...prev, { role: 'assistant', content: data.reply }]);
        loadConversations(tenant, false);
      })
      .catch((e) => setError(String(e)))
      .finally(() => setSending(false));
  };

  if (!farsightUrl) {
    return (
      <PluginPage layout={PageLayoutType.Canvas}>
        <Alert title="Not configured" severity="warning">
          Set the Farsight URL on this plugin&apos;s Configuration page.
        </Alert>
      </PluginPage>
    );
  }

  return (
    <PluginPage layout={PageLayoutType.Canvas}>
      <div className={s.layout}>
        <div className={s.sidebar}>
          <Button
            variant="secondary"
            fill="outline"
            onClick={startNewConversation}
            disabled={!tenant}
            className={s.newButton}
          >
            + New conversation
          </Button>
          <div className={s.conversationList}>
            {conversations.map((c) => (
              <div key={c.id} className={s.conversationRow}>
                <button
                  className={c.id === conversationId ? s.conversationItemActive : s.conversationItem}
                  onClick={() => loadConversation(tenant, c.id)}
                >
                  {c.title || `Conversation #${c.id}`}
                </button>
                <IconButton
                  name="trash-alt"
                  size="sm"
                  tooltip="Delete conversation"
                  onClick={() => deleteConversation(c.id)}
                />
              </div>
            ))}
            {conversations.length === 0 && <div className={s.empty}>No past conversations yet.</div>}
          </div>
        </div>

        <div className={s.container} data-testid={testIds.chat.container}>
          {tenantsError && (
            <Alert title="Could not reach Farsight" severity="error">
              {tenantsError}
            </Alert>
          )}
          {tenants.length > 1 && (
            <div className={s.tenantPicker}>
              <Select
                options={tenants.map((t) => ({ label: t, value: t }))}
                value={tenant}
                onChange={(v) => {
                  const next = v.value || '';
                  setTenant(next);
                  startNewConversation();
                  loadConversations(next, true);
                }}
                width={30}
              />
            </div>
          )}

          <div className={s.messages}>
            {loadingHistory && <LoadingPlaceholder text="Loading conversation..." />}
            {!loadingHistory && messages.length === 0 && (
              <div className={s.empty}>Ask a question about your devices.</div>
            )}
            {!loadingHistory &&
              messages.map((m, i) => (
                <div key={i} className={m.role === 'user' ? s.userMessage : s.assistantMessage}>
                  {m.content}
                </div>
              ))}
            {sending && <LoadingPlaceholder text="Thinking..." />}
          </div>

          {error && (
            <Alert title="Error" severity="error" onRemove={() => setError('')}>
              {error}
            </Alert>
          )}

          <div className={s.inputRow}>
            <TextArea
              className={s.textArea}
              value={input}
              onChange={(e) => setInput(e.currentTarget.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && !e.shiftKey) {
                  e.preventDefault();
                  send();
                }
              }}
              placeholder="Type a message..."
              rows={2}
              disabled={!tenant || sending}
            />
            <Button onClick={send} disabled={!tenant || sending || !input.trim()}>
              Send
            </Button>
          </div>
        </div>
      </div>
    </PluginPage>
  );
}

export default Chat;

const getStyles = (theme: GrafanaTheme2) => ({
  layout: css`
    display: flex;
    gap: ${theme.spacing(2)};
    height: calc(100vh - 160px);
    padding: ${theme.spacing(2)};
  `,
  sidebar: css`
    width: 220px;
    flex-shrink: 0;
    display: flex;
    flex-direction: column;
    gap: ${theme.spacing(1)};
  `,
  newButton: css`
    width: 100%;
  `,
  conversationList: css`
    display: flex;
    flex-direction: column;
    gap: ${theme.spacing(0.5)};
    overflow-y: auto;
  `,
  conversationRow: css`
    display: flex;
    align-items: center;
    gap: ${theme.spacing(0.25)};
    border-radius: ${theme.shape.radius.default};
    &:hover {
      background: ${theme.colors.background.secondary};
    }
  `,
  conversationItem: css`
    flex: 1;
    min-width: 0;
    text-align: left;
    background: transparent;
    border: none;
    color: ${theme.colors.text.primary};
    padding: ${theme.spacing(0.75, 1)};
    border-radius: ${theme.shape.radius.default};
    cursor: pointer;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  `,
  conversationItemActive: css`
    flex: 1;
    min-width: 0;
    text-align: left;
    background: ${theme.colors.background.secondary};
    border: none;
    color: ${theme.colors.text.primary};
    padding: ${theme.spacing(0.75, 1)};
    border-radius: ${theme.shape.radius.default};
    cursor: pointer;
    font-weight: 600;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  `,
  container: css`
    flex: 1;
    display: flex;
    flex-direction: column;
    min-width: 0;
  `,
  tenantPicker: css`
    margin-bottom: ${theme.spacing(2)};
  `,
  messages: css`
    flex: 1;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    gap: ${theme.spacing(1)};
    margin-bottom: ${theme.spacing(2)};
  `,
  empty: css`
    color: ${theme.colors.text.secondary};
  `,
  userMessage: css`
    align-self: flex-end;
    background: ${theme.colors.primary.main};
    color: ${theme.colors.primary.contrastText};
    border-radius: ${theme.shape.radius.default};
    padding: ${theme.spacing(1, 1.5)};
    max-width: 80%;
    white-space: pre-wrap;
  `,
  assistantMessage: css`
    align-self: flex-start;
    background: ${theme.colors.background.secondary};
    border-radius: ${theme.shape.radius.default};
    padding: ${theme.spacing(1, 1.5)};
    max-width: 80%;
    white-space: pre-wrap;
  `,
  inputRow: css`
    display: flex;
    gap: ${theme.spacing(1)};
    align-items: flex-end;
    width: 100%;
  `,
  textArea: css`
    flex: 1;
    width: auto;
    resize: vertical;
  `,
});
