interface PokerCardProps {
  code?: string;
  concealed?: boolean;
}

export function PokerCard({ code = "XX", concealed = false }: PokerCardProps) {
  const rank = concealed || code === "XX" ? "?" : code[0];
  const suit = concealed || code === "XX" ? "\u2022" : code[1];
  const isRed = suit === "h" || suit === "d";

  const baseClasses = "w-[64px] aspect-[3/4] rounded-xl grid place-items-center p-2 shadow-[inset_0_1px_0_rgba(255,255,255,0.6)] text-2xl font-bold";

  const colorClasses = concealed
    ? "bg-gradient-to-b from-[#20362c] to-[#16231e] text-primary"
    : isRed
      ? "bg-gradient-to-b from-[#fff7ea] to-[#eadcc0] text-[#9d1e1e]"
      : "bg-gradient-to-b from-[#fff7ea] to-[#eadcc0] text-[#1d140a]";

  return (
    <div className={`${baseClasses} ${colorClasses}`}>
      <span>{rank}</span>
      <small className="text-base">{suit}</small>
    </div>
  );
}
