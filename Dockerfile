FROM postgres:16-alpine

# Set default environment variables
ENV POSTGRES_DB=passcheck_db
ENV POSTGRES_USER=postgres
ENV POSTGRES_PASSWORD=postgres

# Expose the standard PostgreSQL port
EXPOSE 5432
