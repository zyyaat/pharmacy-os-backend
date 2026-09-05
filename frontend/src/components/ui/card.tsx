export function Card({ children, ...props }: React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div {...props} className="bg-white rounded-lg shadow p-6">
      {children}
    </div>
  )
}
