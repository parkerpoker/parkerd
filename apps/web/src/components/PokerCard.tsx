interface PokerCardProps {
  code?: string;
  concealed?: boolean;
}

export function PokerCard({ code = "XX", concealed = false }: PokerCardProps) {
  const rank = concealed || code === "XX" ? "?" : code[0];
  const suit = concealed || code === "XX" ? "•" : code[1];
  const suitClass =
    suit === "h" || suit === "d" ? "card card-red" : concealed ? "card card-back" : "card";

  return (
    <div className={suitClass}>
      <span>{rank}</span>
      <small>{suit}</small>
    </div>
  );
}

