import { Icon } from './Icon';
import { IconButton } from './ui';
import { dismissNotice, useNotice } from '../lib/notice';

/** The app-wide confirmation banner, pinned under the header. */
export default function NoticeBar() {
  const notice = useNotice();
  return (
    <div className="notice-anchor" role="status" aria-live="polite">
      {notice && (
        <div key={notice.id} className={`notice-bar callout callout-${notice.tone}`}>
          <Icon name={notice.tone === 'ok' ? 'circle-check' : 'info'} size={16} className="callout-icon shrink-0" />
          <span className="min-w-0 flex-1">
            <span className="font-semibold">{notice.title}</span>
            {notice.detail && <span className="ml-1.5 muted">{notice.detail}</span>}
          </span>
          <IconButton icon="x" label="Dismiss" size="sm" onClick={dismissNotice} />
        </div>
      )}
    </div>
  );
}
