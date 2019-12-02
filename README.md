# MUNGE

The munge package.

MUNGE (**M**UNGE **U**id '**N**' **G**id **E**mporium) is an authentication service for creating and validating credentials.

Bevor starting the service as the first time a secret key has to been created.

For creating the key add store it to the file `/etc/munge/munge.key` the following script is provided:

```bash
create-munge-key
```

In the second step you can start und enable the munge service.

```bash
systemctl start munge
systemctl enalbe munge
```
