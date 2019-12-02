# MUNGE

The munge package.

MUNGE (MUNGE Uid 'N' Gid Emporium) is an authentication service for creating and validating credentials.

Bevor starting the service as the first time a secret key has to been created.

For creating the key add store it to the file `/etc/munge/munge.key` the following script is provided:

```
create-munge-key
```

In the second step you can start und enable the munge service.

```
systemctl start munge
systemctl enalbe munge
```
