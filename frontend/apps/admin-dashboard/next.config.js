/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  allowedDevOrigins: ['127.0.0.1', 'localhost', process.env.REPLIT_DEV_DOMAIN].filter(Boolean),
  images: {
    remotePatterns: [
      {
        protocol: "https",
        hostname: "**",
      },
    ],
  },
  async rewrites() {
    return [
      {
        source: '/api/v1/:path*',
        destination: 'http://127.0.0.1:8080/api/v1/:path*',
      },
      {
        source: '/health',
        destination: 'http://127.0.0.1:8080/health',
      },
    ]
  },
};

module.exports = nextConfig;
