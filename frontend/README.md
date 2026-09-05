# Pharmacy OS Frontend

![Next.js](https://img.shields.io/badge/Next.js-16-black?logo=next.js)
![React](https://img.shields.io/badge/React-19-blue?logo=react)
![TypeScript](https://img.shields.io/badge/TypeScript-5.6-blue?logo=typescript)
![Tailwind CSS](https://img.shields.io/badge/Tailwind_CSS-v4-06B6D4?logo=tailwindcss)
![Supabase](https://img.shields.io/badge/Supabase-2.39-3ECF8E?logo=supabase)

Modern pharmacy management system frontend built with Next.js 16, React 19, and Tailwind CSS v4.

## Features

- **Dashboard** - Overview with key metrics and analytics
- **Inventory Management** - Track medications, stock levels, and batches
- **Employee Management** - Manage pharmacy staff and roles
- **Attendance Tracking** - Clock in/out system for employees
- **Branch Management** - Multi-location support
- **Reports** - Generate and view various reports
- **Settings** - Application configuration

## Tech Stack

| Technology | Version | Purpose |
|------------|---------|---------|
| Next.js | 16.x | React Framework |
| React | 19.x | UI Library |
| TypeScript | 5.6.x | Type Safety |
| Tailwind CSS | 4.x | Styling |
| Supabase JS | 2.39.x | Auth & Database |

## Getting Started

### Prerequisites

- Node.js 18+ 
- npm or yarn
- Supabase project (for auth & database)

### Installation

1. Clone the repository:
```bash
git clone https://github.com/zyyaat/pharmacy-os-frontend.git
cd pharmacy-os-frontend
```

2. Install dependencies:
```bash
npm install
```

3. Copy environment variables:
```bash
cp .env.example .env.local
```

4. Update `.env.local` with your Supabase credentials:
```env
NEXT_PUBLIC_SUPABASE_URL=your-supabase-url
NEXT_PUBLIC_SUPABASE_ANON_KEY=your-anon-key
```

5. Run development server:
```bash
npm run dev
```

6. Open [http://localhost:3000](http://localhost:3000)

## Project Structure

```
src/
├── app/                    # Next.js App Router pages
│   ├── (auth)/            # Authentication pages (login)
│   ├── (dashboard)/       # Dashboard pages
│   │   ├── inventory/     # Inventory management
│   │   ├── employees/     # Employee management
│   │   ├── attendance/    # Attendance tracking
│   │   ├── branches/      # Branch management
│   │   ├── reports/       # Reports
│   │   └── settings/      # Settings
│   ├── layout.tsx         # Root layout
│   └── globals.css        # Global styles
├── components/            # Reusable components
│   ├── layout/           # Layout components (header, sidebar)
│   └── ui/               # UI primitives (button, card, input, etc.)
├── hooks/                 # Custom React hooks
│   ├── useAuth.ts        # Authentication hook
│   ├── useEmployees.ts   # Employees data hook
│   ├── useInventory.ts   # Inventory data hook
│   ├── useAttendance.ts  # Attendance data hook
│   └── usePharmacy.ts    # Pharmacy data hook
├── lib/                   # Utilities and configurations
│   ├── supabase.ts       # Supabase client
│   ├── api.ts            # API utilities
│   ├── auth.ts           # Auth helpers
│   ├── validations.ts    # Form validations
│   └── utils.ts          # General utilities
└── types/                 # TypeScript type definitions
    ├── index.ts          # Export all types
    ├── employee.ts       # Employee types
    ├── medication.ts     # Medication types
    ├── pharmacy.ts       # Pharmacy types
    ├── branch.ts         # Branch types
    └── attendance.ts     # Attendance types
```

## Available Scripts

| Command | Description |
|---------|-------------|
| `npm run dev` | Start development server on port 3000 |
| `npm run build` | Build for production |
| `npm run start` | Start production server |
| `npm run lint` | Run ESLint |

## Environment Variables

| Variable | Description | Required |
|----------|-------------|----------|
| `NEXT_PUBLIC_SUPABASE_URL` | Your Supabase project URL | ✅ Yes |
| `NEXT_PUBLIC_SUPABASE_ANON_KEY` | Your Supabase anonymous key | ✅ Yes |
| `NEXT_PUBLIC_API_URL` | Custom backend API URL (optional) | ❌ No |
| `NEXT_PUBLIC_APP_NAME` | Application name | ❌ No |

## Deployment

### Vercel (Recommended)

1. Push your code to GitHub
2. Import project on [vercel.com](https://vercel.com)
3. Add environment variables
4. Deploy!

### Docker

```dockerfile
FROM node:20-alpine AS builder
WORKDIR /app
COPY package*.json ./
RUN npm ci
COPY . .
RUN npm run build

FROM node:20-alpine AS runner
WORKDIR /app
ENV NODE_ENV=production
COPY --from=builder /app/.next/standalone ./
COPY --from=builder /app/.next/static ./.next/static
COPY --from=builder /app/public ./public
EXPOSE 3000
CMD ["node", "server.js"]
```

### Replit

1. Fork this repository to Replit
2. Configure environment variables in Secrets
3. Click "Run"

## Related Repositories

- [Backend (Go + Gin)](https://github.com/zyyaat/pharmacy-os-backend) - Go backend API

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is private. All rights reserved.
