import type { ReactNode } from 'react';

interface PanelProps {
  title?: string;
  children: ReactNode;
  className?: string;
}

export function Panel({ title, children, className = '' }: PanelProps) {
  return (
    <div className={`rounded-lg border border-border bg-card/50 backdrop-blur-sm ${className}`}>
      {title && (
        <div className="border-b border-border px-4 py-3">
          <h3 className="text-sm tracking-wide text-muted-foreground uppercase">{title}</h3>
        </div>
      )}
      <div className="p-4">
        {children}
      </div>
    </div>
  );
}
