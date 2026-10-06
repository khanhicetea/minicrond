import { useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { api, errorText } from '../api';
import { alertChannelsQuery, keys } from '../queries';
import type { AlertChannel } from '../types';

const emptyChannel = (): AlertChannel => ({ name: '', type: 'telegram', chat_id: '', disable_notification: false, batch_window: 10 });

export function AlertChannelsPanel() {
  const channels = useQuery(alertChannelsQuery());
  const client = useQueryClient();
  const [draft, setDraft] = useState<AlertChannel | null>(null);
  const [editing, setEditing] = useState(false);
  const [token, setToken] = useState('');
  const [deleting, setDeleting] = useState<AlertChannel | null>(null);
  const [removeReferences, setRemoveReferences] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');

  const refresh = async () => {
    await Promise.all([
      client.invalidateQueries({ queryKey: keys.alertChannels }),
      client.invalidateQueries({ queryKey: keys.jobs }),
      client.invalidateQueries({ queryKey: ['job'] }),
      client.invalidateQueries({ queryKey: keys.workerStates }),
    ]);
  };
  const edit = (channel?: AlertChannel) => {
    setDraft(channel ? { ...channel } : emptyChannel());
    setEditing(!!channel);
    setToken('');
    setDeleting(null);
    setError('');
    setNotice('');
  };
  const save = async () => {
    if (!draft || busy) return;
    setBusy(true); setError(''); setNotice('');
    try {
      await api.saveAlertChannel(draft.name, {
        type: draft.type, chat_id: draft.chat_id, disable_notification: draft.disable_notification,
        batch_window: draft.batch_window, bot_token: token || undefined,
      });
      setToken(''); setDraft(null);
      await refresh();
      setNotice('Channel saved. New alerts use the updated settings.');
    } catch (err) { setError(errorText(err)); }
    finally { setBusy(false); }
  };
  const remove = async () => {
    if (!deleting || busy) return;
    setBusy(true); setError(''); setNotice('');
    try {
      await api.deleteAlertChannel(deleting.name, removeReferences);
      setDeleting(null);
      await refresh();
      setNotice('Channel deleted. Already queued alerts keep their original settings.');
    } catch (err) { setError(errorText(err)); }
    finally { setBusy(false); }
  };
  const test = async (name: string) => {
    setBusy(true); setError(''); setNotice('');
    try { await api.testAlertChannel(name); setNotice(`Test sent to ${name}.`); }
    catch (err) { setError(errorText(err)); }
    finally { setBusy(false); }
  };

  return (
    <section className="panel p-4 space-y-4">
      <div className="flex items-center justify-between gap-3">
        <h2 className="panel-title">Alert channels</h2>
        <button type="button" className="btn-primary-x" disabled={busy} onClick={() => edit()}>Add channel</button>
      </div>
      <p className="text-xs muted">Telegram settings and bot credentials are stored in the daemon database. Saved tokens are never returned to the browser. Up to 100 channels.</p>
      {channels.isPending && <p className="text-sm muted">Loading channels…</p>}
      {channels.isError && <p role="alert" className="text-sm text-red-400">{errorText(channels.error)}</p>}
      {channels.data?.length === 0 && <p className="text-sm muted">No alert channels yet.</p>}
      <div className="space-y-2">
        {channels.data?.map(channel => (
          <div key={channel.name} className="flex flex-wrap items-center justify-between gap-2 border-b border-base-300 py-2">
            <div><strong className="text-sm">{channel.name}</strong><span className="ml-3 text-xs muted">Telegram · {channel.chat_id} · {channel.batch_window}s batches</span></div>
            <div className="flex gap-2">
              <button type="button" className="btn-sub" disabled={busy} onClick={() => void test(channel.name)}>Test</button>
              <button type="button" className="btn-sub" disabled={busy} onClick={() => edit(channel)}>Edit</button>
              <button type="button" className="btn-sub" disabled={busy} onClick={() => { setDeleting(channel); setDraft(null); setToken(''); setRemoveReferences(false); setError(''); setNotice(''); }}>Delete</button>
            </div>
          </div>
        ))}
      </div>
      {draft && (
        <form className="space-y-3 border border-base-300 rounded-lg p-4" onSubmit={event => { event.preventDefault(); void save(); }}>
          <h3 className="text-sm font-semibold">{editing ? `Edit ${draft.name}` : 'New Telegram channel'}</h3>
          <label className="block field-label">Name
            <input className="mc-input mt-1" value={draft.name} required maxLength={100} pattern="[a-z0-9][a-z0-9_.-]{0,99}" disabled={editing || busy} onChange={event => setDraft({ ...draft, name: event.target.value })} />
          </label>
          <label className="block field-label">Chat ID
            <input className="mc-input mt-1" value={draft.chat_id} required maxLength={256} disabled={busy} onChange={event => setDraft({ ...draft, chat_id: event.target.value })} />
          </label>
          <label className="block field-label">Bot token {editing && '(leave blank to keep saved token)'}
            <input className="mc-input mt-1" type="password" autoComplete="new-password" value={token} required={!editing} maxLength={256} disabled={busy} onChange={event => setToken(event.target.value)} />
          </label>
          <label className="block field-label">Batch window (seconds)
            <input className="mc-input mt-1" type="number" min={1} max={3600} required value={draft.batch_window} disabled={busy} onChange={event => setDraft({ ...draft, batch_window: Number(event.target.value) })} />
          </label>
          <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={draft.disable_notification} disabled={busy} onChange={event => setDraft({ ...draft, disable_notification: event.target.checked })} />Silent Telegram notifications</label>
          <div className="flex gap-2">
            <button className="btn-primary-x" type="submit" disabled={busy}>Save channel</button>
            <button className="btn-sub" type="button" disabled={busy} onClick={() => { setDraft(null); setToken(''); setError(''); }}>Cancel</button>
          </div>
        </form>
      )}
      {deleting && (
        <div className="space-y-3 border border-base-300 rounded-lg p-4">
          <p className="text-sm">Delete <strong>{deleting.name}</strong>?</p>
          <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={removeReferences} disabled={busy} onChange={event => setRemoveReferences(event.target.checked)} />Also remove from job and worker definitions</label>
          <p className="text-xs muted">Without this option, referenced channels cannot be deleted. Config-owned definitions must be edited in their file first. Queued alerts are not cancelled.</p>
          <div className="flex gap-2">
            <button type="button" className="btn-primary-x" disabled={busy} onClick={() => void remove()}>Delete channel</button>
            <button type="button" className="btn-sub" disabled={busy} onClick={() => { setDeleting(null); setError(''); }}>Cancel</button>
          </div>
        </div>
      )}
      {error && <p role="alert" className="text-sm text-red-400">{error}</p>}
      {notice && <p role="status" className="text-sm text-green-400">{notice}</p>}
    </section>
  );
}
