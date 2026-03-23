interface StatusBadgeProps {
  status: 'running' | 'stopped' | 'connected' | 'disconnected' | 'active' | 'idle';
  label?: string;
}

export function StatusBadge({ status, label }: StatusBadgeProps) {
  const colors = {
    running: 'bg-[#2ecc71]/20 text-[#2ecc71] border-[#2ecc71]/30',
    connected: 'bg-[#2ecc71]/20 text-[#2ecc71] border-[#2ecc71]/30',
    active: 'bg-[#2ecc71]/20 text-[#2ecc71] border-[#2ecc71]/30',
    stopped: 'bg-[#e74c3c]/20 text-[#e74c3c] border-[#e74c3c]/30',
    disconnected: 'bg-[#e74c3c]/20 text-[#e74c3c] border-[#e74c3c]/30',
    idle: 'bg-[#f39c12]/20 text-[#f39c12] border-[#f39c12]/30',
  };

  return (
    <div className={`inline-flex items-center gap-2 rounded-md border px-2.5 py-1 text-xs ${colors[status]}`}>
      <div className="h-1.5 w-1.5 rounded-full bg-current"></div>
      <span>{label || status}</span>
    </div>
  );
}
